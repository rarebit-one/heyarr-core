package cas

// deviceOf has no Windows implementation. Windows has no hardlinks across
// volumes and no reflink, so the question this answers — "will materialisation
// degrade to a copy?" — has the same answer whatever it returned, and inventing
// a number would only make the caller believe it had checked.
func deviceOf(_ string) (int64, bool, error) { return 0, false, nil }
