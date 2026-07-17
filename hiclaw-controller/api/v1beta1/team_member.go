package v1beta1

// TeamMemberIsCoordinator reports whether member acts as a team coordinator.
// Empty role defaults to coordinator.
func TeamMemberIsCoordinator(member TeamMemberSpec) bool {
	return member.Role == "" || member.Role == "coordinator"
}
