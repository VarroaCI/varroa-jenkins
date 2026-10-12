package mitev1

// commandTypeLabels names each imperative command type with the word a log
// line, a metric label, and an activity row carry.
var commandTypeLabels = map[CommandType]string{
	CommandTypeSafeRestart:            "safe_restart",
	CommandTypeDeleteItem:             "delete_item",
	CommandTypeReplayWebhook:          "replay_webhook",
	CommandTypeCollectPluginInventory: "collect_plugin_inventory",
	CommandTypeTriggerBuild:           "trigger_build",
	CommandTypeCancelBuild:            "cancel_build",
	CommandTypeResumeBuild:            "resume_build",
	CommandTypeProceedInput:           "proceed_input",
	CommandTypeAbortInput:             "abort_input",
	CommandTypeReplayBuild:            "replay_build",
	CommandTypeRestartBuild:           "restart_build",
	CommandTypeScanMultibranch:        "scan_multibranch",
}

// Label answers the command type's lower-case name. An unspecified or
// unknown type answers "unknown", so a metric label stays bounded.
func (t CommandType) Label() string {
	if label, ok := commandTypeLabels[t]; ok {
		return label
	}
	return "unknown"
}
