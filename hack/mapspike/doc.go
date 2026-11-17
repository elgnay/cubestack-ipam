// Package mapspike is a one-off experiment, not production code.
//
// It exists to answer a single question against a real kube-apiserver: can a
// MutatingAdmissionPolicy inject the layer 2 network into a KubeVirt
// VirtualMachine, so that a VirtualMachinePool member gets its own `<vm>-static`
// NAD reference without anything having to write the member after it is stored?
//
// The question is worth this much ceremony because the two alternatives both cost
// something. The `${vm-name}` token (see the plan at
// ~/.claude/plans/sparkling-hatching-forest.md) needs the controller to patch VMs
// after creation, which opens a window where a member carries an unexpanded
// network name. A mutating webhook of our own needs certs and a deployment. If MAP
// works, it is neither — but "if" is doing real work in that sentence, and the
// behaviour it turns on (JSON Patch append onto an absent array, CEL against a CRD,
// ordering against the mutating webhooks already on this cluster) is not something
// the documentation settles.
//
// Build-tagged so `make test`, which runs `go test $(go list ./... | grep -v /e2e)`,
// never picks it up:
//
//	CGO_ENABLED=0 go test -tags mapspike ./hack/mapspike/... -v
//
// This file is untagged on purpose. Without it the package would contain no files
// under the default constraints, and `go build ./...` reports an error rather than
// skipping the directory.
package mapspike
