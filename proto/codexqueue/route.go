package codexqueue

// RouteIn is everything plan 3.1 reads. ClaimProcs is the number of distinct processes
// holding an owner-proven claim for the thread.
type RouteIn struct {
	PriorUnknown bool
	Hosts        int
	ClaimProcs   int
	Setting      bool
	GateOK       bool
	Immediate    bool
	Verified     bool
	Ask          bool
	AsksEnabled  bool
}

type RouteOut struct {
	Row                   int
	State                 string
	Wake                  bool
	AskRejectedNoEndpoint bool
}

func (in RouteIn) capable() bool {
	return in.Setting && in.ClaimProcs == 1 && in.GateOK && (!in.Ask || in.AsksEnabled)
}

func (in RouteIn) ambiguous() bool {
	return in.Hosts >= 2 || (in.Hosts >= 1 && in.ClaimProcs >= 1) || in.ClaimProcs >= 2
}

// Matches lists every row whose condition holds; the table is correct only if it is
// always exactly one.
func Matches(in RouteIn) []int {
	if in.PriorUnknown {
		return []int{1}
	}
	var m []int
	if in.Hosts == 1 && in.ClaimProcs == 0 {
		m = append(m, 2)
	}
	if in.ambiguous() {
		m = append(m, 6)
	}
	if in.Hosts == 0 && in.ClaimProcs <= 1 {
		switch {
		case !in.capable():
			m = append(m, 3)
		case !in.Immediate || !in.Verified:
			m = append(m, 4)
		default:
			m = append(m, 5)
		}
	}
	return m
}

func Select(in RouteIn) RouteOut {
	row := Matches(in)[0]
	switch row {
	case 1:
		return RouteOut{Row: 1, State: "unknown"}
	case 2:
		return RouteOut{Row: 2, State: "as-is"}
	case 4:
		return RouteOut{Row: 4, State: "mailboxed"}
	case 5:
		return RouteOut{Row: 5, State: "mailboxed", Wake: true}
	}
	return RouteOut{Row: row, State: "unreachable", AskRejectedNoEndpoint: in.Ask}
}
