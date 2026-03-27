package main

func meanFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func countWhere(frames []frameSnapshot, fn func(frameSnapshot) bool) int {
	n := 0
	for _, f := range frames {
		if fn(f) {
			n++
		}
	}
	return n
}
