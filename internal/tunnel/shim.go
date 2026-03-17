package tunnel

// DeployShims will deploy tunnel shims (op, xclip wrappers) into VMs so that
// standard tools inside the VM transparently delegate to their host-side
// counterparts over the forwarded tunnel ports.
//
// Implementation is deferred to the provisioning engine integration phase.
func DeployShims(profile string, available []DiscoveryResult) error {
	// TODO: implement shim deployment during provisioning
	return nil
}
