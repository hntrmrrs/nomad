package scheduler

import (
	"fmt"
	"log"
	"math/rand"
	"reflect"
	"regexp"

	"github.com/hashicorp/nomad/nomad/structs"
)

var (
	// Regex to capture the identifier of a task group name.
	taskGroupID = regexp.MustCompile(`.+\..+\[(.*)\]`)
)

// allocTuple is a tuple of the allocation name and potential alloc ID
type allocTuple struct {
	Name      string
	TaskGroup *structs.TaskGroup
	Alloc     *structs.Allocation
}

// materializeTaskGroups is used to materialize all the task groups
// a job requires. This is used to do the count expansion.
func materializeTaskGroups(job *structs.Job) map[string]*structs.TaskGroup {
	out := make(map[string]*structs.TaskGroup)
	for _, tg := range job.TaskGroups {
		for i := 0; i < tg.Count; i++ {
			name := fmt.Sprintf("%s.%s[%d]", job.Name, tg.Name, i)
			out[name] = tg
		}
	}
	return out
}

// materializeSystemTaskGroups is used to materialize all the task groups
// a system job requires. This is used to do the node expansion.
func materializeSystemTaskGroups(job *structs.Job, nodes []*structs.Node) map[string]*structs.TaskGroup {
	out := make(map[string]*structs.TaskGroup)
	for _, tg := range job.TaskGroups {
		for _, node := range nodes {
			name := fmt.Sprintf("%s.%s[%s]", job.Name, tg.Name, node.ID)
			out[name] = tg
		}
	}
	return out
}

// extractTaskGroupIdreturns the unique identifier for the task group
// name. It returns the id that distinguishes multiple instantiations of a task
// group. In the case of the system scheduler they will  be the nodes name and
// otherwise it will be the tasks count.
func extractTaskGroupId(name string) (string, error) {
	matches := taskGroupID.FindStringSubmatch(name)
	if len(matches) != 2 {
		return "", fmt.Errorf("could not determine task group id from %v: %#v", name, matches)
	}

	return matches[1], nil
}

// diffResult is used to return the sets that result from the diff
type diffResult struct {
	place, update, migrate, stop, ignore []allocTuple
}

func (d *diffResult) GoString() string {
	return fmt.Sprintf("allocs: (place %d) (update %d) (migrate %d) (stop %d) (ignore %d)",
		len(d.place), len(d.update), len(d.migrate), len(d.stop), len(d.ignore))
}

// diffAllocs is used to do a set difference between the target allocations
// and the existing allocations. This returns 5 sets of results, the list of
// named task groups that need to be placed (no existing allocation), the
// allocations that need to be updated (job definition is newer), allocs that
// need to be migrated (node is draining), the allocs that need to be evicted
// (no longer required), and those that should be ignored.
func diffAllocs(job *structs.Job, taintedNodes map[string]bool,
	required map[string]*structs.TaskGroup, allocs []*structs.Allocation) *diffResult {
	result := &diffResult{}

	// Scan the existing updates
	existing := make(map[string]struct{})
	for _, exist := range allocs {
		// Index the existing node
		name := exist.Name
		existing[name] = struct{}{}

		// Check for the definition in the required set
		tg, ok := required[name]

		// If not required, we stop the alloc
		if !ok {
			result.stop = append(result.stop, allocTuple{
				Name:      name,
				TaskGroup: tg,
				Alloc:     exist,
			})
			continue
		}

		// If we are on a tainted node, we must migrate
		if taintedNodes[exist.NodeID] {
			result.migrate = append(result.migrate, allocTuple{
				Name:      name,
				TaskGroup: tg,
				Alloc:     exist,
			})
			continue
		}

		// If the definition is updated we need to update
		// XXX: This is an extremely conservative approach. We can check
		// if the job definition has changed in a way that affects
		// this allocation and potentially ignore it.
		if job.ModifyIndex != exist.Job.ModifyIndex {
			result.update = append(result.update, allocTuple{
				Name:      name,
				TaskGroup: tg,
				Alloc:     exist,
			})
			continue
		}

		// Everything is up-to-date
		result.ignore = append(result.ignore, allocTuple{
			Name:      name,
			TaskGroup: tg,
			Alloc:     exist,
		})
	}

	// Scan the required groups
	for name, tg := range required {
		// Check for an existing allocation
		_, ok := existing[name]

		// Require a placement if no existing allocation. If there
		// is an existing allocation, we would have checked for a potential
		// update or ignore above.
		if !ok {
			result.place = append(result.place, allocTuple{
				Name:      name,
				TaskGroup: tg,
			})
		}
	}
	return result
}

// readyNodesInDCs returns all the ready nodes in the given datacenters
func readyNodesInDCs(state State, dcs []string) ([]*structs.Node, error) {
	// Index the DCs
	dcMap := make(map[string]struct{}, len(dcs))
	for _, dc := range dcs {
		dcMap[dc] = struct{}{}
	}

	// Scan the nodes
	var out []*structs.Node
	iter, err := state.Nodes()
	if err != nil {
		return nil, err
	}
	for {
		raw := iter.Next()
		if raw == nil {
			break
		}

		// Filter on datacenter and status
		node := raw.(*structs.Node)
		if node.Status != structs.NodeStatusReady {
			continue
		}
		if node.Drain {
			continue
		}
		if _, ok := dcMap[node.Datacenter]; !ok {
			continue
		}
		out = append(out, node)
	}
	return out, nil
}

// retryMax is used to retry a callback until it returns success or
// a maximum number of attempts is reached
func retryMax(max int, cb func() (bool, error)) error {
	attempts := 0
	for attempts < max {
		done, err := cb()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		attempts += 1
	}
	return &SetStatusError{
		Err:        fmt.Errorf("maximum attempts reached (%d)", max),
		EvalStatus: structs.EvalStatusFailed,
	}
}

// taintedNodes is used to scan the allocations and then check if the
// underlying nodes are tainted, and should force a migration of the allocation.
func taintedNodes(state State, allocs []*structs.Allocation) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, alloc := range allocs {
		if _, ok := out[alloc.NodeID]; ok {
			continue
		}

		node, err := state.NodeByID(alloc.NodeID)
		if err != nil {
			return nil, err
		}

		// If the node does not exist, we should migrate
		if node == nil {
			out[alloc.NodeID] = true
			continue
		}

		out[alloc.NodeID] = structs.ShouldDrainNode(node.Status) || node.Drain
	}
	return out, nil
}

// shuffleNodes randomizes the slice order with the Fisher-Yates algorithm
func shuffleNodes(nodes []*structs.Node) {
	n := len(nodes)
	for i := n - 1; i > 0; i-- {
		j := rand.Intn(i + 1)
		nodes[i], nodes[j] = nodes[j], nodes[i]
	}
}

// tasksUpdated does a diff between task groups to see if the
// tasks, their drivers or config have updated.
func tasksUpdated(a, b *structs.TaskGroup) bool {
	// If the number of tasks do not match, clearly there is an update
	if len(a.Tasks) != len(b.Tasks) {
		return true
	}

	// Check each task
	for _, at := range a.Tasks {
		bt := b.LookupTask(at.Name)
		if bt == nil {
			return true
		}
		if at.Driver != bt.Driver {
			return true
		}
		if !reflect.DeepEqual(at.Config, bt.Config) {
			return true
		}

		// Inspect the network to see if the dynamic ports are different
		if len(at.Resources.Networks) != len(bt.Resources.Networks) {
			return true
		}
		for idx := range at.Resources.Networks {
			an := at.Resources.Networks[idx]
			bn := bt.Resources.Networks[idx]
			if len(an.DynamicPorts) != len(bn.DynamicPorts) {
				return true
			}
		}
	}
	return false
}

// setStatus is used to update the status of the evaluation
func setStatus(logger *log.Logger, planner Planner, eval, nextEval *structs.Evaluation, status, desc string) error {
	logger.Printf("[DEBUG] sched: %#v: setting status to %s", eval, status)
	newEval := eval.Copy()
	newEval.Status = status
	newEval.StatusDescription = desc
	if nextEval != nil {
		newEval.NextEval = nextEval.ID
	}
	return planner.UpdateEval(newEval)
}

// inplaceUpdate attempts to update allocations in-place where possible.
func inplaceUpdate(ctx Context, eval *structs.Evaluation, job *structs.Job,
	stack Stack, updates []allocTuple) []allocTuple {

	n := len(updates)
	inplace := 0
	for i := 0; i < n; i++ {
		// Get the update
		update := updates[i]

		// Check if the task drivers or config has changed, requires
		// a rolling upgrade since that cannot be done in-place.
		existing := update.Alloc.Job.LookupTaskGroup(update.TaskGroup.Name)
		if tasksUpdated(update.TaskGroup, existing) {
			continue
		}

		// Get the existing node
		node, err := ctx.State().NodeByID(update.Alloc.NodeID)
		if err != nil {
			ctx.Logger().Printf("[ERR] sched: %#v failed to get node '%s': %v",
				eval, update.Alloc.NodeID, err)
			continue
		}
		if node == nil {
			continue
		}

		// Set the existing node as the base set
		stack.SetNodes([]*structs.Node{node})

		// Stage an eviction of the current allocation
		ctx.Plan().AppendUpdate(update.Alloc, structs.AllocDesiredStatusStop,
			allocInPlace)

		// Attempt to match the task group
		option, size := stack.Select(update.TaskGroup)

		// Pop the allocation
		ctx.Plan().PopUpdate(update.Alloc)

		// Skip if we could not do an in-place update
		if option == nil {
			continue
		}

		// Restore the network offers from the existing allocation.
		// We do not allow network resources (reserved/dynamic ports)
		// to be updated. This is guarded in taskUpdated, so we can
		// safely restore those here.
		for task, resources := range option.TaskResources {
			existing := update.Alloc.TaskResources[task]
			resources.Networks = existing.Networks
		}

		// Create a shallow copy
		newAlloc := new(structs.Allocation)
		*newAlloc = *update.Alloc

		// Update the allocation
		newAlloc.EvalID = eval.ID
		newAlloc.Job = job
		newAlloc.Resources = size
		newAlloc.TaskResources = option.TaskResources
		newAlloc.Metrics = ctx.Metrics()
		newAlloc.DesiredStatus = structs.AllocDesiredStatusRun
		newAlloc.ClientStatus = structs.AllocClientStatusPending
		ctx.Plan().AppendAlloc(newAlloc)

		// Remove this allocation from the slice
		updates[i] = updates[n-1]
		i--
		n--
		inplace++
	}
	if len(updates) > 0 {
		ctx.Logger().Printf("[DEBUG] sched: %#v: %d in-place updates of %d", eval, inplace, len(updates))
	}
	return updates[:n]
}

// evictAndPlace is used to mark allocations for evicts and add them to the
// placement queue. evictAndPlace modifies both the the diffResult and the
// limit. It returns true if the limit has been reached.
func evictAndPlace(ctx Context, diff *diffResult, allocs []allocTuple, desc string, limit *int) bool {
	n := len(allocs)
	for i := 0; i < n && i < *limit; i++ {
		a := allocs[i]
		ctx.Plan().AppendUpdate(a.Alloc, structs.AllocDesiredStatusStop, desc)
		diff.place = append(diff.place, a)
	}
	if n <= *limit {
		*limit -= n
		return false
	}
	*limit = 0
	return true
}
