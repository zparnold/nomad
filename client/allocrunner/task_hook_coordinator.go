package allocrunner

import (
	"context"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TaskHookCoordinator helps coordinate when mainTasks start tasks can launch
// namely after all Prestart Tasks have run, and after all BlockUntilCompleted have completed
type taskHookCoordinator struct {
	logger hclog.Logger

	// constant for quickly starting all prestart tasks
	closedCh chan struct{}

	// Each context is used to gate task runners launching the tasks. A task
	// runner waits until the context associated its lifecycle context is
	// done/cancelled.
	mainTaskCtx           context.Context
	mainTaskCtxCancel     context.CancelFunc
	poststopTaskCtx       context.Context
	poststopTaskCtxCancel context.CancelFunc

	prestartSidecarTasks   map[string]struct{}
	prestartEphemeralTasks map[string]struct{}
	mainTasks              map[string]struct{}
	poststopTasks          map[string]struct{}
}

func newTaskHookCoordinator(logger hclog.Logger, tasks []*structs.Task) *taskHookCoordinator {
	closedCh := make(chan struct{})
	close(closedCh)

	mainTaskCtx, mainTaskCancelFn := context.WithCancel(context.Background())
	poststopTaskCtx, poststopTaskCancelFn := context.WithCancel(context.Background())

	c := &taskHookCoordinator{
		logger:                 logger,
		closedCh:               closedCh,
		mainTaskCtx:            mainTaskCtx,
		mainTaskCtxCancel:      mainTaskCancelFn,
		poststopTaskCtx:        poststopTaskCtx,
		poststopTaskCtxCancel:  poststopTaskCancelFn,
		prestartSidecarTasks:   map[string]struct{}{},
		prestartEphemeralTasks: map[string]struct{}{},
		mainTasks:              map[string]struct{}{},
		poststopTasks:          map[string]struct{}{},
	}
	c.setTasks(tasks)
	return c
}

func (c *taskHookCoordinator) setTasks(tasks []*structs.Task) {
	for _, task := range tasks {

		if task.Lifecycle == nil {
			c.mainTasks[task.Name] = struct{}{}
			continue
		}

		switch task.Lifecycle.Hook {
		case structs.TaskLifecycleHookPrestart:
			if task.Lifecycle.Sidecar {
				c.prestartSidecarTasks[task.Name] = struct{}{}
			} else {
				c.prestartEphemeralTasks[task.Name] = struct{}{}
			}
		case structs.TaskLifecycleHookPoststop:
			c.poststopTasks[task.Name] = struct{}{}
		default:
			c.logger.Error("invalid lifecycle hook", "hook", task.Lifecycle.Hook)
		}
	}

	if !c.hasPrestartTasks() {
		c.mainTaskCtxCancel()
	}
	if !c.hasMainTasks() {
		c.poststopTaskCtxCancel()
	}

}

func (c *taskHookCoordinator) hasPrestartTasks() bool {
	return len(c.prestartSidecarTasks)+len(c.prestartEphemeralTasks) > 0
}

func (c *taskHookCoordinator) hasMainTasks() bool {
	return len(c.mainTasks) > 0
}

func (c *taskHookCoordinator) startConditionForTask(task *structs.Task) <-chan struct{} {
	if task.Lifecycle == nil {
		return c.mainTaskCtx.Done()
	}
	switch task.Lifecycle.Hook {
	case structs.TaskLifecycleHookPrestart:
		// Prestart tasks start without checking status of other tasks
		return c.closedCh
	case structs.TaskLifecycleHookPoststop:
		return c.poststopTaskCtx.Done()
	default:
		return c.mainTaskCtx.Done()
	}
}

// This is not thread safe! This must only be called from one thread per alloc runner.
func (c *taskHookCoordinator) taskStateUpdated(states map[string]*structs.TaskState) {
	if c.mainTaskCtx.Err() != nil && c.poststopTaskCtx.Err() != nil {
		// nothing to do here
		return
	}

	for task := range c.prestartSidecarTasks {
		st := states[task]
		if st == nil || st.StartedAt.IsZero() {
			continue
		}

		delete(c.prestartSidecarTasks, task)
	}

	for task := range c.prestartEphemeralTasks {
		st := states[task]
		if st == nil || !st.Successful() {
			continue
		}

		delete(c.prestartEphemeralTasks, task)
	}

	for task := range c.mainTasks {
		st := states[task]
		if st == nil || !st.Successful() {
			continue
		}

		delete(c.mainTasks, task)
	}

	for task := range c.poststopTasks {
		st := states[task]
		if st == nil || !st.Successful() {
			continue
		}

		delete(c.poststopTasks, task)
	}

	// everything well
	if !c.hasPrestartTasks() {
		c.mainTaskCtxCancel()
	}

	if !c.hasMainTasks() {
		c.poststopTaskCtxCancel()
	}
}
