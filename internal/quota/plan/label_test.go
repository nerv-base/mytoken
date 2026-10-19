package plan

import "testing"

func TestPlanLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"self_serve_business_prolite", "Business Prolite"},
		{"personal_professional_trial", "Personal Professional Trial"},
		{"SELF_SERVE_TEAM", "Team"},
		{"credits", ""},
		{"Credits", ""},
		{"self_serve_credits", ""},
		{"   ", ""},
		{"", ""},
		{"体验版", "体验版"},
		{"max_5x", "Max 5x"},
		{"5x_pro", "5x Pro"},
		{"pro-max", "Pro Max"},
		{"api_beta", "Api Beta"},
	}
	for _, tc := range cases {
		if got := PlanLabel(tc.in); got != tc.want {
			t.Errorf("PlanLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
