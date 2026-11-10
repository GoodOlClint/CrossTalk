package codexqueue

import "testing"

func TestRoute_PartitionExhaustive_ExpectOneRowPerInputAndGuardAlone(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for hosts := 0; hosts <= 3; hosts++ {
			for procs := 0; procs <= 3; procs++ {
				for _, setting := range []bool{false, true} {
					for _, gate := range []bool{false, true} {
						for _, immediate := range []bool{false, true} {
							for _, verified := range []bool{false, true} {
								for _, ask := range []bool{false, true} {
									in := RouteIn{PriorUnknown: prior, Hosts: hosts, ClaimProcs: procs, Setting: setting, GateOK: gate, Immediate: immediate, Verified: verified, Ask: ask}
									m := Matches(in)
									if len(m) != 1 {
										t.Fatalf("%+v matched rows %v, want exactly one", in, m)
									}
									if prior && m[0] != 1 {
										t.Fatalf("%+v: prior unknown must match only row 1, got %v", in, m)
									}
									if !prior && m[0] == 1 {
										t.Fatalf("%+v: row 1 matched without a prior unknown", in)
									}
									if got := Select(in).Row; got != m[0] {
										t.Fatalf("%+v: Select row %d != Matches %d", in, got, m[0])
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestRoute_Rows(t *testing.T) {
	base := RouteIn{Setting: true, GateOK: true, Immediate: true, Verified: true, ClaimProcs: 1}
	with := func(f func(*RouteIn)) RouteIn { x := base; f(&x); return x }
	cases := []struct {
		name          string
		in            RouteIn
		row           int
		state         string
		wake          bool
		askRejectedNE bool
	}{
		{"Row1_PriorUnknownEvenWithOneHost_ExpectUnknownNoWake", with(func(x *RouteIn) { x.PriorUnknown = true; x.Hosts = 1; x.ClaimProcs = 0 }), 1, "unknown", false, false},
		{"Row1_PriorUnknownWithCapability_ExpectUnknownNoWake", with(func(x *RouteIn) { x.PriorUnknown = true }), 1, "unknown", false, false},
		{"Row2_OneHostNoClaim_ExpectAppServerUnchanged", with(func(x *RouteIn) { x.Hosts = 1; x.ClaimProcs = 0 }), 2, "as-is", false, false},
		{"Row3_SettingOff_ExpectUnreachableAskRejected", with(func(x *RouteIn) { x.Setting = false; x.Ask = true }), 3, "unreachable", false, true},
		{"Row3_NoClaim_ExpectUnreachable", with(func(x *RouteIn) { x.ClaimProcs = 0 }), 3, "unreachable", false, false},
		{"Row3_GateFails_ExpectUnreachable", with(func(x *RouteIn) { x.GateOK = false }), 3, "unreachable", false, false},
		{"Row3_NextTurnWithoutCapability_ExpectUnreachableNotMailboxed", with(func(x *RouteIn) { x.Immediate = false; x.Setting = false }), 3, "unreachable", false, false},
		{"Row3_UnverifiedWithoutCapability_ExpectUnreachableNotMailboxed", with(func(x *RouteIn) { x.Verified = false; x.ClaimProcs = 0 }), 3, "unreachable", false, false},
		{"Row3_AskBeforeAsksEnabled_ExpectRejectedEvenWithCapability", with(func(x *RouteIn) { x.Ask = true }), 3, "unreachable", false, true},
		{"Row4_NextTurnWithCapability_ExpectMailboxedNoWake", with(func(x *RouteIn) { x.Immediate = false }), 4, "mailboxed", false, false},
		{"Row4_UnverifiedWithCapability_ExpectMailboxedNoWake", with(func(x *RouteIn) { x.Verified = false }), 4, "mailboxed", false, false},
		{"Row5_ImmediateVerifiedWithCapability_ExpectMailboxedAndWake", base, 5, "mailboxed", true, false},
		{"Row5_AskAfterAsksEnabled_ExpectMailboxedAndWake", with(func(x *RouteIn) { x.Ask = true; x.AsksEnabled = true }), 5, "mailboxed", true, false},
		{"Row6_TwoHosts_ExpectUnreachable", with(func(x *RouteIn) { x.Hosts = 2; x.ClaimProcs = 0; x.Ask = true }), 6, "unreachable", false, true},
		{"Row6_HostAndOwnerProvenClaim_ExpectUnreachableAmbiguousOwner", with(func(x *RouteIn) { x.Hosts = 1 }), 6, "unreachable", false, false},
		{"Row6_ClaimsFromTwoProcesses_ExpectUnreachableAmbiguousOwner", with(func(x *RouteIn) { x.ClaimProcs = 2 }), 6, "unreachable", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := Select(c.in)
			if o.Row != c.row || o.State != c.state || o.Wake != c.wake || o.AskRejectedNoEndpoint != c.askRejectedNE {
				t.Fatalf("got %+v, want row %d state %s wake %v askRejected %v", o, c.row, c.state, c.wake, c.askRejectedNE)
			}
		})
	}
}
