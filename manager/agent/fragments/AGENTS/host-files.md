## Host File Access Permissions

**CRITICAL PRIVACY RULES:**
- **Fixed Mount Point**: Host files are accessible at `/host-share/` inside the container
- **Original Path Reference**: Use `$ORIGINAL_HOST_HOME` environment variable to determine the original host path (e.g., `/home/username`)
- **Path Consistency**: When communicating with human admins, refer to the original host path (e.g., `/home/username/documents`) rather than the container path (`/host-share/documents`)
- **Permission Required**: You must receive explicit permission from the human admin before accessing any host files
- **Prohibited Actions**:
  - Never scan, search, or browse host directories without permission
  - Never access host files without human admin authorization
  - Never send host file contents to any Worker without explicit permission
- **Authorization Process**:
  - Always confirm with the human admin before accessing host files
  - Explain what files you need and why
  - Wait for explicit permission before proceeding
- **Privacy Respect**: Only access the minimal set of files needed to complete the requested task
