package main

import (
	"github.com/goobers/goobers/internal/controlplane/scheduling"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
)

func schedulerStateFiles() scheduling.StateFiles {
	return scheduling.StateFiles{ClaimLock: withClaimLock, FileLock: withFileLock, BoundedLock: withBoundedFileLock}
}

func fileStateStore(l instance.Layout) (stateclient.Store, error) {
	return schedulerStateFiles().File(l)
}
func stageStateStore(l instance.Layout) (stateclient.Store, error) {
	return schedulerStateFiles().Stage(l)
}
func newDaemonStateService(l instance.Layout) (*scheduling.StateService, error) {
	return schedulerStateFiles().NewService(l)
}

var openStageStateStore = stageStateStore
var openHeldStageStateStore = scheduling.HeldStageStateStore
var heldStateStore = scheduling.HeldStateStore
var statePlaneSelected = scheduling.StatePlaneSelected
var stateContext = scheduling.StateContext
var runBelongsToGaggle = scheduling.RunBelongsToGaggle

type daemonStateService = scheduling.StateService

const stateLockOperationPostMergeUpdate = scheduling.StateLockOperationPostMergeUpdate
const stateLockOperationSiblingUpdate = scheduling.StateLockOperationSiblingUpdate
const backlogHealthCursorLockFile = scheduling.BacklogHealthCursorLockFile
const stateLockOperationBacklogHealthCursor = scheduling.StateLockOperationBacklogHealthCursor
