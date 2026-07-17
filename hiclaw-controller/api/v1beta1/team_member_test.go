package v1beta1

import "testing"

func TestTeamMemberIsCoordinator(t *testing.T) {
	tests := []struct {
		name string
		role string
		want bool
	}{
		{name: "empty role defaults coordinator", role: "", want: true},
		{name: "coordinator role", role: "coordinator", want: true},
		{name: "member role", role: "member", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			member := TeamMemberSpec{Name: "human", Role: tt.role}
			if got := TeamMemberIsCoordinator(member); got != tt.want {
				t.Fatalf("TeamMemberIsCoordinator() = %v, want %v", got, tt.want)
			}
		})
	}
}
