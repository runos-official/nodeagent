package preflight

import "testing"

// Goal 27, W2. THE ESCAPE HATCH IS WHAT LETS A RUNOS NODE BE REINSTALLED AT ALL.
//
// The existing-k8s preflight BLOCKS an install when it finds a CNI config it does not recognise,
// and that block is deliberately EARLIER than the wipe that would have cleared it. So a config
// RunOS put there itself has to be recognised, or the node becomes uninstallable by its own
// leftovers, and the failure is a refusal to start rather than anything that looks like a bug.
//
// It used to depend on the word "cilium" appearing somewhere in the file. For Multus that was
// luck: Multus in auto mode writes 00-multus.conf by embedding the existing default CNI config as
// its delegate, and that embedded copy normally mentions Cilium. A change in Multus's generated
// format would have made every node that had VM networking uninstallable.

func TestRunosOwnPluginsAreRecognisedByName(t *testing.T) {
	for _, name := range []string{
		"05-cilium.conflist",
		"00-multus.conf",
		"10-MULTUS.CONF",
	} {
		if !isRunosCNIName(lower(name)) {
			t.Errorf("%s is a config RunOS installs and must not read as foreign", name)
		}
	}
}

func TestAForeignPluginIsNotRecognisedByName(t *testing.T) {
	for _, name := range []string{
		"05-foreign-flannel.conflist",
		"99-foreign-leftover.conf",
		"10-calico.conflist",
		"87-podman-bridge.conflist",
	} {
		if isRunosCNIName(lower(name)) {
			t.Errorf("%s is not something RunOS installs and must read as foreign", name)
		}
	}
}

func TestMultusAutoConfigIsRecognisedWhateverItsDelegateSays(t *testing.T) {
	// The realistic case: Multus embeds the default CNI config as its delegate, so the file
	// mentions Cilium and would have passed by luck.
	withCilium := `{"name":"multus-cni-network","type":"multus","delegates":[{"name":"cilium","type":"cilium-cni"}]}`
	if !isRunosCNIBody(withCilium) {
		t.Error("a Multus config delegating to cilium must be recognised")
	}

	// THE CASE THAT USED TO BREAK: the same file with a delegate that never says "cilium". Before
	// naming Multus outright, this blocked the install on RunOS's own config.
	withoutCilium := `{"name":"multus-cni-network","type":"multus","delegates":[{"name":"default-cni-network","plugins":[{"type":"portmap"}]}]}`
	if !isRunosCNIBody(withoutCilium) {
		t.Error("a Multus config must be recognised on its own name, not on what its delegate happens to contain")
	}
}

func TestAForeignBodyIsStillForeign(t *testing.T) {
	// The planted config from the 2026-08-14 hardware attempt, which is what W2 exists for.
	flannel := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel","delegate":{"hairpinMode":true}}]}`
	if isRunosCNIBody(flannel) {
		t.Error("a foreign flannel config must read as foreign")
	}
	if isRunosCNIBody(`{"type":"bridge","name":"podman"}`) {
		t.Error("a podman bridge config must read as foreign")
	}
}

// lower mirrors what the caller does before the name check, so the test exercises the same input.
func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
