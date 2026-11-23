package sim

// RunAll runs every storm and returns their results in catalogue order.
func RunAll(storms []Storm) ([]Result, error) {
	out := make([]Result, 0, len(storms))
	for _, st := range storms {
		res, err := Run(st)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}
