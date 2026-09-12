package recordstore

// WithTestBarrier returns cfg with its maintenance-round test barrier
// set to barrier. It exists only in test builds, so no production
// caller can reach the unexported field it sets.
func WithTestBarrier(cfg MaintenanceConfig, barrier func()) MaintenanceConfig {
	cfg.testBarrier = barrier
	return cfg
}
