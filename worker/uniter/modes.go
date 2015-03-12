// Copyright 2012-2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package uniter

import (
	"fmt"
	"time"

	"github.com/juju/errors"
	"github.com/juju/utils/featureflag"
	"gopkg.in/juju/charm.v4"
	"gopkg.in/juju/charm.v4/hooks"
	"launchpad.net/tomb"

	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/feature"
	"github.com/juju/juju/state/watcher"
	"github.com/juju/juju/worker"
	"github.com/juju/juju/worker/uniter/operation"
)

// Mode defines the signature of the functions that implement the possible
// states of a running Uniter.
type Mode func(u *Uniter) (Mode, error)

// ModeContinue determines what action to take based on persistent uniter state.
func ModeContinue(u *Uniter) (next Mode, err error) {
	defer modeContext("ModeContinue", &err)()
	opState := u.operationState()

	// Resume interrupted deployment operations.
	if opState.Kind == operation.Install {
		logger.Infof("resuming charm install")
		return ModeInstalling(opState.CharmURL)
	} else if opState.Kind == operation.Upgrade {
		logger.Infof("resuming charm upgrade")
		return ModeUpgrading(opState.CharmURL), nil
	}

	// If we got this far, we should have an installed charm,
	// so initialize the metrics collector according to what's
	// currently deployed.
	if err := u.initializeMetricsCollector(); err != nil {
		return nil, errors.Trace(err)
	}

	if featureflag.Enabled(feature.LeaderElection) {
		// Check for any leadership change, and enact it if possible. (This may
		// fail if we attempt to become leader while we should be in a hook error
		// mode); this is mildly inconvenient, but not a problem, because we'll
		// be watching for leader election (and deposition) in all the loop modes
		// that can handle them anyway.
		logger.Infof("checking leadership status")

		// NOTE: the wait looks scary, but a ClaimLeadership ticket should always
		// complete quickly; worst-case is API latency time, but it's designed that
		// it should be vanishingly rare to hit that code path. (Make it impossible?)
		isLeader := u.leadershipTracker.ClaimLeader().Wait()
		if isLeader == opState.Leader {
			logger.Infof("leadership status is up-to-date")
		} else {
			creator := newResignLeadershipOp()
			if isLeader {
				creator = newAcceptLeadershipOp()
			}
			err := u.runOperation(creator)
			if err == nil {
				return ModeContinue, nil
			} else if errors.Cause(err) != operation.ErrCannotAcceptLeadership {
				return nil, errors.Trace(err)
			}
			logger.Infof("cannot accept leadership yet, choosing next mode")
		}
	}

	var creator creator
	switch opState.Kind {
	case operation.RunAction:
		// TODO(fwereade): we *should* handle interrupted actions, and make sure
		// they're marked as failed, but that's not for now.
		logger.Infof("found incomplete action %q; ignoring", opState.ActionId)
		logger.Infof("recommitting prior %q hook", opState.Hook.Kind)
		creator = newSkipHookOp(*opState.Hook)
	case operation.RunHook:
		switch opState.Step {
		case operation.Pending:
			logger.Infof("awaiting error resolution for %q hook", opState.Hook.Kind)
			return ModeHookError, nil
		case operation.Queued:
			logger.Infof("found queued %q hook", opState.Hook.Kind)
			creator = newRunHookOp(*opState.Hook)
		case operation.Done:
			logger.Infof("committing %q hook", opState.Hook.Kind)
			creator = newSkipHookOp(*opState.Hook)
		}
	case operation.Continue:
		logger.Infof("continuing after %q hook", opState.Hook.Kind)
		if opState.Hook.Kind == hooks.Stop {
			return ModeTerminating, nil
		}
		return ModeAbide, nil
	default:
		return nil, errors.Errorf("unknown operation kind %v", opState.Kind)
	}
	return continueAfter(u, creator)
}

// ModeInstalling is responsible for the initial charm deployment. If an install
// operation were to set an appropriate status, it shouldn't be necessary; but see
// ModeUpgrading for discussion relevant to both.
func ModeInstalling(curl *charm.URL) (next Mode, err error) {
	name := fmt.Sprintf("ModeInstalling %s", curl)
	return func(u *Uniter) (next Mode, err error) {
		defer modeContext(name, &err)()
		// TODO(fwereade) 2015-01-19
		// This SetStatus call should probably be inside the operation somehow;
		// which in turn implies that the SetStatus call in PrepareHook is
		// also misplaced, and should also be explicitly part of the operation.
		if err = u.unit.SetAgentStatus(params.StatusInstalling, "", nil); err != nil {
			return nil, errors.Trace(err)
		}
		return continueAfter(u, newInstallOp(curl))
	}, nil
}

// ModeUpgrading is responsible for upgrading the charm. It shouldn't really
// need to be a mode at all -- it's just running a single operation -- but
// it's not safe to call it inside arbitrary other modes, because failing to
// pass through ModeContinue on the way out could cause a queued hook to be
// accidentally skipped.
func ModeUpgrading(curl *charm.URL) Mode {
	name := fmt.Sprintf("ModeUpgrading %s", curl)
	return func(u *Uniter) (next Mode, err error) {
		defer modeContext(name, &err)()
		return continueAfter(u, newUpgradeOp(curl))
	}
}

// ModeTerminating marks the unit dead and returns ErrTerminateAgent.
func ModeTerminating(u *Uniter) (next Mode, err error) {
	defer modeContext("ModeTerminating", &err)()
	if err = u.unit.SetAgentStatus(params.StatusStopping, "", nil); err != nil {
		return nil, errors.Trace(err)
	}
	w, err := u.unit.Watch()
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer watcher.Stop(w, &u.tomb)
	for {
		select {
		case <-u.tomb.Dying():
			return nil, tomb.ErrDying
		case actionId := <-u.f.ActionEvents():
			creator := newActionOp(actionId)
			if err := u.runOperation(creator); err != nil {
				return nil, errors.Trace(err)
			}
		case _, ok := <-w.Changes():
			if !ok {
				return nil, watcher.EnsureErr(w)
			}
			if err := u.unit.Refresh(); err != nil {
				return nil, errors.Trace(err)
			}
			if hasSubs, err := u.unit.HasSubordinates(); err != nil {
				return nil, errors.Trace(err)
			} else if hasSubs {
				continue
			}
			// The unit is known to be Dying; so if it didn't have subordinates
			// just above, it can't acquire new ones before this call.
			if err := u.unit.EnsureDead(); err != nil {
				return nil, errors.Trace(err)
			}
			return nil, worker.ErrTerminateAgent
		}
	}
}

// ModeAbide is the Uniter's usual steady state. It watches for and responds to:
// * service configuration changes
// * charm upgrade requests
// * relation changes
// * unit death
// * acquisition or loss of service leadership
func ModeAbide(u *Uniter) (next Mode, err error) {
	defer modeContext("ModeAbide", &err)()
	opState := u.operationState()
	if opState.Kind != operation.Continue {
		return nil, errors.Errorf("insane uniter state: %#v", opState)
	}
	if err := u.deployer.Fix(); err != nil {
		return nil, errors.Trace(err)
	}

	if featureflag.Enabled(feature.LeaderElection) {
		// This behaviour is essentially modelled on that of config-changed.
		// Note that we ask for events *before* we run the hook, so that the
		// event-discard caused by running the hook resets the events correctly.
		// And we don't `return continueAfter(...` for the same reason -- we
		// need to have started watching for events before we run the guaranteed
		// one.
		u.f.WantLeaderSettingsEvents(!opState.Leader)
		if !opState.Leader && !u.ranLeaderSettingsChanged {
			// TODO(fwereade): define in charm/hooks
			creator := newSimpleRunHookOp(hooks.Kind("leader-settings-changed"))
			if err := u.runOperation(creator); err != nil {
				return nil, errors.Trace(err)
			}
		}
	} else {
		u.f.WantLeaderSettingsEvents(false)
	}

	if !u.ranConfigChanged {
		return continueAfter(u, newSimpleRunHookOp(hooks.ConfigChanged))
	}
	if !opState.Started {
		return continueAfter(u, newSimpleRunHookOp(hooks.Start))
	}
	if err = u.unit.SetAgentStatus(params.StatusActive, "", nil); err != nil {
		return nil, errors.Trace(err)
	}
	u.f.WantUpgradeEvent(false)
	u.relations.StartHooks()
	defer func() {
		if e := u.relations.StopHooks(); e != nil {
			if err == nil {
				err = e
			} else {
				logger.Errorf("error while stopping hooks: %v", e)
			}
		}
	}()

	select {
	case <-u.f.UnitDying():
		return modeAbideDyingLoop(u)
	default:
	}
	return modeAbideAliveLoop(u)
}

// modeAbideAliveLoop handles all state changes for ModeAbide when the unit
// is in an Alive state.
func modeAbideAliveLoop(u *Uniter) (Mode, error) {
	var leaderElected, leaderDeposed <-chan struct{}
	for {
		if featureflag.Enabled(feature.LeaderElection) {
			if leaderElected == nil && leaderDeposed == nil {
				if u.operationState().Leader {
					leaderDeposed = u.leadershipTracker.WaitMinion().Ready()
				} else {
					leaderElected = u.leadershipTracker.WaitLeader().Ready()
				}
			}
		}
		lastCollectMetrics := time.Unix(u.operationState().CollectMetricsTime, 0)
		collectMetricsSignal := u.collectMetricsAt(
			time.Now(), lastCollectMetrics, metricsPollInterval,
		)
		var creator creator
		select {
		case <-u.tomb.Dying():
			return nil, tomb.ErrDying
		case <-u.f.UnitDying():
			return modeAbideDyingLoop(u)
		case curl := <-u.f.UpgradeEvents():
			return ModeUpgrading(curl), nil
		case ids := <-u.f.RelationsEvents():
			creator = newUpdateRelationsOp(ids)
		case actionId := <-u.f.ActionEvents():
			creator = newActionOp(actionId)
		case tags := <-u.f.StorageEvents():
			creator = newUpdateStorageOp(tags)
		case <-u.f.ConfigEvents():
			creator = newSimpleRunHookOp(hooks.ConfigChanged)
		case <-u.f.MeterStatusEvents():
			creator = newSimpleRunHookOp(hooks.MeterStatusChanged)
		case <-collectMetricsSignal:
			creator = newSimpleRunHookOp(hooks.CollectMetrics)
		case hookInfo := <-u.relations.Hooks():
			creator = newRunHookOp(hookInfo)
		case hookInfo := <-u.storage.Hooks():
			creator = newRunHookOp(hookInfo)
		case <-leaderElected:
			leaderElected = nil
			creator = newAcceptLeadershipOp()
		case <-leaderDeposed:
			leaderDeposed = nil
			creator = newResignLeadershipOp()
		case <-u.f.LeaderSettingsEvents():
			// TODO(fwereade): define in charm/hooks
			creator = newSimpleRunHookOp(hooks.Kind("leader-settings-changed"))
		}
		if err := u.runOperation(creator); err != nil {
			return nil, errors.Trace(err)
		}
	}
}

// modeAbideDyingLoop handles the proper termination of all relations in
// response to a Dying unit.
func modeAbideDyingLoop(u *Uniter) (next Mode, err error) {
	if err := u.unit.Refresh(); err != nil {
		return nil, errors.Trace(err)
	}
	if err = u.unit.DestroyAllSubordinates(); err != nil {
		return nil, errors.Trace(err)
	}
	if err := u.relations.SetDying(); err != nil {
		return nil, errors.Trace(err)
	}
	if featureflag.Enabled(feature.LeaderElection) {
		if u.operationState().Leader {
			if err := u.runOperation(newResignLeadershipOp()); err != nil {
				return nil, errors.Trace(err)
			}
			// So far as the charm knows, we've resigned leadership... but that
			// won't actually happen until at least 30s after the leadership tracker
			// is shut down, and that won't be for a while yet. In the meantime,
			// is-leader calls will continue to return true, reflecting reality and
			// preserving their guarantees; it just needs to be clear that even if
			// this happens, we *will not* run a further leader-deposed hook.
		}
	}
	for {
		if len(u.relations.GetInfo()) == 0 {
			return continueAfter(u, newSimpleRunHookOp(hooks.Stop))
		}
		var creator creator
		select {
		case <-u.tomb.Dying():
			return nil, tomb.ErrDying
		case actionId := <-u.f.ActionEvents():
			creator = newActionOp(actionId)
		case <-u.f.ConfigEvents():
			creator = newSimpleRunHookOp(hooks.ConfigChanged)
		case <-u.f.LeaderSettingsEvents():
			// TODO(fwereade): define in charm/hooks
			creator = newSimpleRunHookOp(hooks.Kind("leader-settings-changed"))
		case hookInfo := <-u.relations.Hooks():
			creator = newRunHookOp(hookInfo)
		}
		if err := u.runOperation(creator); err != nil {
			return nil, errors.Trace(err)
		}
	}
}

// ModeHookError is responsible for watching and responding to:
// * user resolution of hook errors
// * forced charm upgrade requests
// * loss of service leadership
func ModeHookError(u *Uniter) (next Mode, err error) {
	defer modeContext("ModeHookError", &err)()
	opState := u.operationState()
	if opState.Kind != operation.RunHook || opState.Step != operation.Pending {
		return nil, errors.Errorf("insane uniter state: %#v", u.operationState())
	}

	// Create error information for status.
	hookInfo := *opState.Hook
	hookName := string(hookInfo.Kind)
	statusData := map[string]interface{}{}
	if hookInfo.Kind.IsRelation() {
		statusData["relation-id"] = hookInfo.RelationId
		if hookInfo.RemoteUnit != "" {
			statusData["remote-unit"] = hookInfo.RemoteUnit
		}
		relationName, err := u.relations.Name(hookInfo.RelationId)
		if err != nil {
			return nil, errors.Trace(err)
		}
		hookName = fmt.Sprintf("%s-%s", relationName, hookInfo.Kind)
	}
	statusData["hook"] = hookName
	statusMessage := fmt.Sprintf("hook failed: %q", hookName)

	// Run the select loop.
	u.f.WantResolvedEvent()
	u.f.WantUpgradeEvent(true)
	var leaderDeposed <-chan struct{}
	if featureflag.Enabled(feature.LeaderElection) {
		if opState.Leader {
			leaderDeposed = u.leadershipTracker.WaitMinion().Ready()
		}
	}
	for {
		// We set status inside the loop so we can be sure we *reset* status after a
		// failed re-execute of the current hook (which will set Active while rerunning
		// it) or a leader-deposed (which won't, but better safe than sorry).
		if err = u.unit.SetAgentStatus(params.StatusError, statusMessage, statusData); err != nil {
			return nil, errors.Trace(err)
		}
		select {
		case <-u.tomb.Dying():
			return nil, tomb.ErrDying
		case curl := <-u.f.UpgradeEvents():
			return ModeUpgrading(curl), nil
		case rm := <-u.f.ResolvedEvents():
			var creator creator
			switch rm {
			case params.ResolvedRetryHooks:
				creator = newRetryHookOp(hookInfo)
			case params.ResolvedNoHooks:
				creator = newSkipHookOp(hookInfo)
			default:
				return nil, errors.Errorf("unknown resolved mode %q", rm)
			}
			err := u.runOperation(creator)
			if errors.Cause(err) == operation.ErrHookFailed {
				continue
			} else if err != nil {
				return nil, errors.Trace(err)
			}
			return ModeContinue, nil
		case <-leaderDeposed:
			// This should trigger at most once -- we can't reaccept leadership while
			// in an error state.
			leaderDeposed = nil
			if err := u.runOperation(newResignLeadershipOp()); err != nil {
				return nil, errors.Trace(err)
			}
		}
	}
}

// ModeConflicted is responsible for watching and responding to:
// * user resolution of charm upgrade conflicts
// * forced charm upgrade requests
func ModeConflicted(curl *charm.URL) Mode {
	return func(u *Uniter) (next Mode, err error) {
		defer modeContext("ModeConflicted", &err)()
		// TODO(mue) Add helpful data here too in later CL.
		if err = u.unit.SetAgentStatus(params.StatusError, "upgrade failed", nil); err != nil {
			return nil, errors.Trace(err)
		}
		u.f.WantResolvedEvent()
		u.f.WantUpgradeEvent(true)
		var creator creator
		select {
		case <-u.tomb.Dying():
			return nil, tomb.ErrDying
		case curl = <-u.f.UpgradeEvents():
			creator = newRevertUpgradeOp(curl)
		case <-u.f.ResolvedEvents():
			creator = newResolvedUpgradeOp(curl)
		}
		return continueAfter(u, creator)
	}
}

// modeContext returns a function that implements logging and common error
// manipulation for Mode funcs.
func modeContext(name string, err *error) func() {
	logger.Infof("%s starting", name)
	return func() {
		logger.Infof("%s exiting", name)
		*err = errors.Annotatef(*err, name)
	}
}

// continueAfter is commonly used at the end of a Mode func to execute the
// operation returned by creator and return ModeContinue (or any error).
func continueAfter(u *Uniter, creator creator) (Mode, error) {
	if err := u.runOperation(creator); err != nil {
		return nil, errors.Trace(err)
	}
	return ModeContinue, nil
}
