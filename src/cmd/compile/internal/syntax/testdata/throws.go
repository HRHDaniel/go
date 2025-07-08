package p

func a() error {
	x, ^ := functionWithMultipleReturns()
	^ = functionWithError()
	z := x ^ x
	z ^= x
	x, _ = functionWithMultipleReturns()
	x, err := functionWithMultipleReturns()
	if err != nil {
		return
	}
}

func b() (int, error) {
	x, ^ := functionWithMultipleReturns()
	^ = functionWithError()
	z := x ^ x
	z ^= x
	x, _ = functionWithMultipleReturns()
	x, err := functionWithMultipleReturns()
	if err != nil {
		return 0, err
	}
}

func functionWithError() error {
	return nil
}

func functionWithMultipleReturns() (int, error) {
	return 0, nil
}
