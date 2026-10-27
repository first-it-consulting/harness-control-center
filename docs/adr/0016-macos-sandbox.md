# ADR-0016: Agents run locally on macOS with Apple's Virtualization framework

- Status: accepted
- Date: 2026-10-26

## Context

The first thing most people try is running an agent on their own
computer, and most of them use a Mac. Our sandbox (ADR-0001) is a
Firecracker microVM, which needs Linux with KVM, so today a Mac user must
first set up a Linux server. That is the biggest hurdle for newcomers.

macOS has its own hypervisor, used through Apple's Virtualization
framework (VZ). It runs Linux guests in separate hardware-virtualized
VMs, as an ordinary user, on every Apple Silicon Mac. We considered:

1. **A hidden Linux VM** running our normal runner, with Firecracker
   nested inside. One code path, but it needs nested virtualization (M3
   or newer, macOS 15 or newer), keeps a VM running all the time, and
   still needs root inside that VM.
2. **Apple's `container` tool** (macOS 26), one VM per container. Quick
   to wire up, but a dependency on a separate tool whose network
   controls we would have to match.
3. **A native sandbox provider** using VZ directly: one VM per job,
   started by the runner.

## Decision

We add a second sandbox provider, `vz`, for macOS on Apple Silicon. The
runner, the policy engine, the egress proxy and the guest images' inits
stay the same; only the provider differs.

- **One VM per run**, started by the runner process itself, as the user
  who runs it. There is **no privileged component at all** on macOS: no
  helper, no sudo. CPUs and memory are the VM's; the run's timeout and
  the kill switch stop it (`Stop`, which ends it at once).
- **No network device.** The guest has no network interface. Its only
  channel to the outside is one virtio socket (vsock) port, on which the
  provider serves the job's egress proxy. The guest's init forwards
  `127.0.0.1:3128` to that port (`hcc-guest vsock-proxy`), so the agent
  sees the same proxy variables as under Firecracker. There is no route,
  no DNS and no other host service the guest could reach, so the
  firewall that Firecracker needs has no counterpart here.
- **The same guest contract.** Drives in the same order (read-only root
  file system, task, tool catalog, plugin bundle, writable results disk),
  settings on the kernel command line, the console as the event stream.
  The writable disk is a sparse file the runner reads after the guest
  shut itself down, as before.
- **A guest that reboots is restarted by VZ**, unlike Firecracker, which
  stops. A restarted guest would run its agent again, so the images'
  inits power off on VZ (`hcc.shutdown=poweroff`), and a kernel panic
  halts the guest (`panic=0`) until the run's timeout instead of
  rebooting it.
- **Our own kernel.** VZ boots an uncompressed arm64 kernel. Distribution
  kernels need loadable modules (and an initramfs) for disks and file
  systems, so we build a small kernel ourselves from a pinned
  kernel.org long-term release with a pinned configuration: the virtio
  devices VZ provides built in, and **module loading disabled**, so
  nothing in the guest can add kernel code.
- **arm64 images.** The images are the same as on Linux, built for arm64
  (Alpine aarch64, Claude Code's arm64 musl build). Every release builds
  them on an arm64 runner from the pinned inputs (`hack/vz/inputs.sh`)
  and attaches them, covered by the release's signed checksums, with the
  kernel's source and configuration (the kernel is GPL-2.0). The Apple
  Silicon build of `hcc` carries their checksums itself, so it can verify
  what it downloads without trusting the download location. **Claude
  Code is not in the release images**: it is Anthropic's software, not
  ours to redistribute, so `hcc` fetches it on the user's Mac from the
  npm registry, checked against a pinned integrity value. Developers
  can still build complete images in a Linux container
  (`hack/vz/build-images.sh`).
- **The runner runs as the user**, its socket and state in the user's
  own directory (owner-only), and the Control Center reaches it through
  that socket (`target: local`).
- **Code signing without an Apple Developer account.** VZ requires the
  `com.apple.security.virtualization` entitlement. An ad-hoc signature
  may carry it, so `make build` signs `hcc` that way on macOS, and so
  will the release pipeline. Who built a release is proven by our
  cosign signature, not by Apple; notarization (which only removes the
  Gatekeeper warning for files downloaded with a browser) can come
  later. Homebrew and command-line downloads are not affected.

### Setup

`hcc local up` (and the Control Center's button for it) sets the
runner up with nothing but `hcc` itself, step by step, and only does
what is not done yet:

1. It downloads the release's images over https and checks each against
   the checksums inside `hcc`. A download that does not match is deleted,
   never used. Images are unpacked as sparse files.
2. It fetches Claude Code from the npm registry and checks it against its
   pinned integrity value. Then it boots a **setup VM** from the `base`
   image: no network, the base image read-only as root, the package
   read-only, and a fresh copy of the `claude-code` image writable. That
   VM copies the binary in and checks that it runs. macOS cannot write
   ext4 itself, and this way no Docker and no privileges are needed. The
   finished image replaces the old one in one rename.
3. It writes `runner.yaml` (owner-only) unless one exists: the user's
   settings win.
4. It installs the runner as a launchd agent of the user's: it starts at
   login and again after a crash, as the user, never as root, and its log
   is in the same directory. `hcc local down` removes it.
5. It waits until the runner answers, then adds it to the Control Center
   as `local`.

### A model on the runner's own machine

The egress proxy refuses every address of the runner's own machine, so
an agent cannot reach services on its host. On a Mac, the model server
(oMLX, Ollama, LM Studio) usually runs on that very machine. The proxy
therefore allows exactly one exception: **the harness's model endpoint**
(its host and port, which the proxy brokers and meters anyway) may
resolve to an address of the runner's machine, if the harness sets
`allowPrivateNetworks`. Loopback addresses stay blocked (the model
server must listen on the machine's network address), and every other
host or port on the machine stays refused.

## Consequences

- Mac users can run agents with `target: local` and no server. The
  isolation is a hardware-virtualized VM, as with Firecracker, and the
  guest has even less network surface. What Firecracker's jailer adds
  (chroot, seccomp on the VMM, cgroups) is replaced by Apple's own
  sandboxing of its VM service process, which we do not control.
- There are two providers to test and keep equal. The guest contract
  (drives, kernel arguments, console markers) is the shared interface;
  both providers' end-to-end tests run the same guest modes.
- Tests that boot VMs need a signed test binary on a Mac
  (`hack/vz/codesign-exec.sh` as `go test -exec`); CI cannot run them
  (no nested virtualization on hosted macOS runners), so they run on a
  developer's Mac before release.
- The Mac build needs cgo (VZ is an Objective-C framework): macOS
  release binaries must be built on macOS.
- Intel Macs are not supported.
