package jsonvalue

import "encoding/json"

// CompareNumbers orders exact JSON decimals without converting to a float or
// expanding an exponent. The result is -1, 0 or 1; invalid tokens return false.
func CompareNumbers(a, b json.Number) (int, bool) {
	an, ac, ae, aok := numberParts(a)
	bn, bc, be, bok := numberParts(b)
	if !aok || !bok {
		return 0, false
	}
	if ac == "0" && bc == "0" {
		return 0, true
	}
	if an != bn {
		if an {
			return -1, true
		}
		return 1, true
	}
	order := 0
	switch {
	case ac == "0":
		order = -1
	case bc == "0":
		order = 1
	default:
		order = compareExponent(shiftExponent(ae, len(ac)), shiftExponent(be, len(bc)))
		if order == 0 {
			for i := 0; i < len(ac) || i < len(bc); i++ {
				ad, bd := byte('0'), byte('0')
				if i < len(ac) {
					ad = ac[i]
				}
				if i < len(bc) {
					bd = bc[i]
				}
				if ad < bd {
					order = -1
					break
				}
				if ad > bd {
					order = 1
					break
				}
			}
		}
	}
	if an {
		order = -order
	}
	return order, true
}

func compareExponent(a, b string) int {
	an, bn := a[0] == '-', b[0] == '-'
	if an != bn {
		if an {
			return -1
		}
		return 1
	}
	if an {
		a, b = a[1:], b[1:]
	}
	order := 0
	if len(a) < len(b) || len(a) == len(b) && a < b {
		order = -1
	}
	if len(a) > len(b) || len(a) == len(b) && a > b {
		order = 1
	}
	if an {
		order = -order
	}
	return order
}
