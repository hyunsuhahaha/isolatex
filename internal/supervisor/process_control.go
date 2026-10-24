package supervisor

// Each worker generation owns its process lifetime control. signal stops that
// generation; close releases platform resources and disables future signals.
type processControl struct {
	signal func(force bool)
	close  func()
}
