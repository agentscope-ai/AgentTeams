import { registerNativeTask } from "./native-task.mjs";

export default {
  id: "agentteams-teamharness",
  name: "TeamHarness",
  version: "0.1.0",
  description: "Native SDK binding for the common AgentTeams Task protocol.",
  register(api) {
    registerNativeTask(api);
  },
};
