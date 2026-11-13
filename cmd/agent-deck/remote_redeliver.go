package main

func isSessionRedeliverArgs(args []string) bool {
	return len(args) >= 2 && args[0] == "session" && args[1] == "redeliver"
}
