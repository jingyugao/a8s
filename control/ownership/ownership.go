package ownership

import (
	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control/resources"
)

// Close changes only the persistent routing view. The controller must separately
// close the old Agentc binding and drain requests already admitted by it.
func Close(state *resources.State) { state.Target.AdmissionOpen = false }

// Commit is used only by the lifecycle controller and saved by aggregate CAS.
// Epoch is a request fence; it never replaces actual writer release evidence.
func Commit(state *resources.State, op *v1.Operation, instance *v1.Instance, release *v1.Release) error {
	old := state.Instances[state.Deployment.ActiveInstanceId]
	if old != nil && !old.WriterReleased {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_READY, "old writer release is unconfirmed")
	}
	if !instance.Ready || instance.Epoch != state.Target.Epoch+1 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "new instance is not ready for the next epoch")
	}
	state.Deployment.ActiveInstanceId = instance.Id
	state.Deployment.ActiveReleaseId = release.Id
	state.Deployment.ObservedGeneration = op.Generation
	state.Target.InstanceId = instance.Id
	state.Target.AgentcBinding = instance.AgentcBinding
	state.Target.Epoch = instance.Epoch
	state.Target.AdmissionOpen = true
	state.Target.Capabilities = append([]string(nil), instance.Capabilities...)
	state.Target.Condition = nil
	op.Published = true
	return nil
}

func Offline(state *resources.State, op *v1.Operation) {
	state.Deployment.ActiveInstanceId = ""
	state.Deployment.ObservedGeneration = op.Generation
	state.Target.InstanceId = ""
	state.Target.AgentcBinding = ""
	state.Target.AdmissionOpen = false
	state.Target.Capabilities = nil
	state.Target.Condition = nil
	op.Published = true
}
