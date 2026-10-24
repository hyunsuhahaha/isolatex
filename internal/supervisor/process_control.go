package supervisor

// Each worker generation owns its process lifetime control. close releases OS
// resources after the direct child has been reaped and removes descendants.
type processControl struct {
	signal func(force bool)
	close  func()
}
