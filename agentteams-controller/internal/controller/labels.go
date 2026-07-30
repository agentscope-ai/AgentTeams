package controller

// mergeLabels layers an arbitrary number of label maps low-to-high so that
// entries in later layers overwrite entries in earlier ones on key
// collision. Nil and empty layers are skipped. The returned map is always
// freshly allocated; inputs are never mutated.
//
// This is the single chokepoint through which every reconciler (Worker,
// Manager, Team) builds the Pod label set pushed into
// backend.CreateRequest.Labels, which downstream becomes
// PodOverlay.Labels at the agent pod-template layer. Keeping the merge
// logic here (rather than scattered across controllers) guarantees the
// documented four-layer priority order:
//
//	pod-template (lowest)  -- merged by ApplyPodTemplate
//	CR metadata.labels
//	CR spec.labels
//	controller system labels  (highest, reserved keys always win)
//
// is enforced identically for every CR kind, and that adding a new layer
// in the future only requires an additional mergeLabels argument at the
// call sites.
func mergeLabels(layers ...map[string]string) map[string]string {
	size := 0
	for _, l := range layers {
		size += len(l)
	}
	if size == 0 {
		return nil
	}
	out := make(map[string]string, size)
	for _, layer := range layers {
		for k, v := range layer {
			out[k] = v
		}
	}
	return out
}
