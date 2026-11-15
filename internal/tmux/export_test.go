package tmux

// SetCachedSubstateForTest seeds the cached substate so tests can exercise
// layers that read it without classifying a real pane. It lives in a _test.go
// file so it never ships in the binary (issue #2986 item 6).
func (s *Session) SetCachedSubstateForTest(sub Substate, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSubstate, s.lastSubstateDetail = sub, detail
}

// resetServerGuardState clears all cached guard state. Tests only.
func resetServerGuardState() {
	socketHealth.Lock()
	socketHealth.byPath = nil
	socketHealth.ino = nil
	socketHealth.inoChanged = nil
	socketHealth.vanished = nil
	socketHealth.firstVanish = nil
	socketHealth.recovering = nil
	socketHealth.Unlock()
	serverAlerts.Lock()
	serverAlerts.last = nil
	serverAlerts.Unlock()
}

// SetSocketGuardForTest lets tests in other packages force the suspect-socket
// verdict and capture operator alerts, with recovery disabled. It returns a
// restore func. Never call it outside tests.
func SetSocketGuardForTest(suspect func(path string) (bool, string), notify func(ServerRecoveryEvent)) (restore func()) {
	prevSuspect, prevNotify, prevAsync := socketSuspectOverride, notifyServerEvent, runRecoveryAsync
	socketSuspectOverride = suspect
	if notify != nil {
		notifyServerEvent = notify
	}
	runRecoveryAsync = func(func()) {}
	reportSync = true
	resetServerGuardState()
	return func() {
		socketSuspectOverride, notifyServerEvent, runRecoveryAsync = prevSuspect, prevNotify, prevAsync
		reportSync = false
		resetServerGuardState()
	}
}
