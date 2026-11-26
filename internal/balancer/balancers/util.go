package balancers

// norm returns v if v is positive, else the original default.
func norm(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
