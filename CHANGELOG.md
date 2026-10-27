# Changelog

## [0.1.1](https://github.com/first-it-consulting/harness-control-center/compare/v0.1.0...v0.1.1) (2026-10-27)


### Features

* **credentials:** edit the credential store, write-only (ADR-0017) ([#7](https://github.com/first-it-consulting/harness-control-center/issues/7)) ([0174a72](https://github.com/first-it-consulting/harness-control-center/commit/0174a72c3d4298cc300666ed5d664861ffdd3d50))
* **local:** hcc local up sets up agents on this Mac ([#5](https://github.com/first-it-consulting/harness-control-center/issues/5)) ([58565bb](https://github.com/first-it-consulting/harness-control-center/commit/58565bb5800598e8505a6f45adb3e5b66dcb1fc5))
* **release:** Apple Silicon build with the macOS sandbox, and its VM images ([#4](https://github.com/first-it-consulting/harness-control-center/issues/4)) ([1cca66d](https://github.com/first-it-consulting/harness-control-center/commit/1cca66def228c88a238a5e8e68d632fb33b99926))
* run agents locally on macOS with Apple's Virtualization framework ([88addbb](https://github.com/first-it-consulting/harness-control-center/commit/88addbbafda100b68aea27a6a89b73e31e79a3b6))
* **web:** a Credentials page for every runner (ADR-0017) ([#8](https://github.com/first-it-consulting/harness-control-center/issues/8)) ([fa81671](https://github.com/first-it-consulting/harness-control-center/commit/fa81671fd0cde40e81b03159a7a3133eb95e1c10))
* **web:** set up this Mac's runner with one button ([#6](https://github.com/first-it-consulting/harness-control-center/issues/6)) ([5775cda](https://github.com/first-it-consulting/harness-control-center/commit/5775cdaaf98bd300cb4be28c60256eadd1768b5d))


### Bug Fixes

* **runner:** a cron run that ends at once no longer stops its schedule ([4f5d8a3](https://github.com/first-it-consulting/harness-control-center/commit/4f5d8a3adbf09206fc94ae98818c5b1c8f20f19c))

## 0.1.0 (2026-10-26)


### Features

* **a2a:** agents call A2A agents; close MCP gate bypasses ([abc724f](https://github.com/first-it-consulting/harness-control-center/commit/abc724fca18802c00f84bde6220f7d53dcf9ef95))
* **approvals:** approval gates, decided in the CLI and web UI ([47e88bc](https://github.com/first-it-consulting/harness-control-center/commit/47e88bc7812cc16a103e037873bf7e9b6ae209b8))
* **broker:** credential broker with per-job CA ([2915723](https://github.com/first-it-consulting/harness-control-center/commit/29157233f8cbe24dc5905eb757a15909ed27c7de))
* **budget:** enforce token and cost budgets at the egress proxy ([5e657db](https://github.com/first-it-consulting/harness-control-center/commit/5e657db172f4d9c171287c105fa0acbb58372d30))
* **cli:** runner daemon, hcc run and job commands over ssh ([0d9f3e7](https://github.com/first-it-consulting/harness-control-center/commit/0d9f3e7ca2c329b221acf6311a4c281bb458c0d0))
* **control:** web UI for harnesses, runners and jobs ([aaea1a1](https://github.com/first-it-consulting/harness-control-center/commit/aaea1a1b8aae3e9109e6b83ef87b12f06c433cc6))
* **egress:** approval gates at the proxy; stop all hosts on budget ([6ddeed3](https://github.com/first-it-consulting/harness-control-center/commit/6ddeed3258b4b43dd7f07c21ae9e6fe47d2244b5))
* **hack:** Firecracker proof of concept under the jailer ([8a49347](https://github.com/first-it-consulting/harness-control-center/commit/8a49347eff24ed73e9cc02f40ffd7d259c83f5eb))
* initial scaffold with harness spec, policy engine and CLI ([fffd828](https://github.com/first-it-consulting/harness-control-center/commit/fffd8281d01e3c5d8979529c919708e8783778fc))
* **mcp:** agents use MCP servers; the Control Center is a read-only one ([7a0b88e](https://github.com/first-it-consulting/harness-control-center/commit/7a0b88e05817e5b1172598cafed25fc8ba2d3f7d))
* **network:** per-VM egress proxy and guarded sandbox links ([9794ff0](https://github.com/first-it-consulting/harness-control-center/commit/9794ff05743f0a12aae851d63fc8dc0af1eaadf8))
* **plugins:** plugins that run in sandboxes of their own ([1bcf933](https://github.com/first-it-consulting/harness-control-center/commit/1bcf93398169b270e7b9b0949117a6f8b83e7af3))
* **results:** hand a job's files back as a sanitized archive ([d15a6cc](https://github.com/first-it-consulting/harness-control-center/commit/d15a6ccfeda737635bfef502527a319b9abb14a0))
* **runner:** persist job history, recover after a crash, prune old jobs ([37a1bab](https://github.com/first-it-consulting/harness-control-center/commit/37a1bab76139dd10ab87a7cee22539f518fa9d27))
* **runner:** runner skeleton with state machine and kill switch ([48cee00](https://github.com/first-it-consulting/harness-control-center/commit/48cee000b4323408adf4220cbcfb3563fb6c2a51))
* **runtime:** Claude Code in the sandbox, with model endpoints ([f0b7e32](https://github.com/first-it-consulting/harness-control-center/commit/f0b7e3260ad9cd4c94b0002f697c0daf564068f2))
* **sandbox:** Firecracker sandbox provider with kill switch ([c337a1b](https://github.com/first-it-consulting/harness-control-center/commit/c337a1b950b1490c8fa372cc5e8592c192b73b8d))
* **schedules:** cron schedules and loops, run by the runner ([fcb1e81](https://github.com/first-it-consulting/harness-control-center/commit/fcb1e8117f9362359f08ba590168b7d119082d36))
* **schedules:** pause schedules when nobody has been in contact ([11f6691](https://github.com/first-it-consulting/harness-control-center/commit/11f6691354691f4ae46a0ffbc48955fc87196846))
* **security:** dedicated runner user and privileged helper ([8765708](https://github.com/first-it-consulting/harness-control-center/commit/8765708b2d257f50d8f5435aa49f8b4ac2f6ff33))
* **subagents:** helper agents in their own sandboxes ([f3b31b0](https://github.com/first-it-consulting/harness-control-center/commit/f3b31b01485752a931384565fe24b0a8eab69d47))
* **tools:** install tools from a signed catalog on the runner ([b345ca2](https://github.com/first-it-consulting/harness-control-center/commit/b345ca274205018fd38bcc66ecadc49b66d86983))


### Bug Fixes

* **approvals:** no upgrades or unshowable requests; loop end race ([884ab75](https://github.com/first-it-consulting/harness-control-center/commit/884ab7575f6e92677ea6f547a0f72c8c4c5f04e1))
* **budget:** fail closed when model usage cannot be metered ([ba98488](https://github.com/first-it-consulting/harness-control-center/commit/ba98488606ba3f85cb01c42b7c7f875b90f863a7))
* **cli:** show newlines and tabs in one-line runner messages ([8b60307](https://github.com/first-it-consulting/harness-control-center/commit/8b60307b7beb7c5e97edfa69802576e169e58113))
* **control:** no session cookie; header sessions and download tickets ([090b156](https://github.com/first-it-consulting/harness-control-center/commit/090b1565e1f30e9880eb35a789efb34fe23bc112))
* **egress:** final usage needs EOF; bound max_tokens charges ([8b6ae8d](https://github.com/first-it-consulting/harness-control-center/commit/8b6ae8dc9338e1600e8dbb89b248ce5bf9eca8f3))
* **egress:** meter by default and require final usage ([90c020f](https://github.com/first-it-consulting/harness-control-center/commit/90c020ff23616619837a43c3ee3cdd4049042de1))
* **egress:** meter only the model endpoint ([dc2271f](https://github.com/first-it-consulting/harness-control-center/commit/dc2271fad7dc27409efd9065516a63e41310d77a))
* **gates:** bodyless requests to gated MCP/A2A hosts may only read ([5ddaf09](https://github.com/first-it-consulting/harness-control-center/commit/5ddaf0955eb2917fe8155cdad6b3dc52566317cd))
* **gates:** fold JSON keys like Go; unknown A2A methods count as sending ([0c45ac4](https://github.com/first-it-consulting/harness-control-center/commit/0c45ac4a1f4ddf9abbaac9bd60864a1729b1aba5))
* **helper:** portable, lint-clean block size conversion ([5153eb1](https://github.com/first-it-consulting/harness-control-center/commit/5153eb1f093283956a91bf248454f9d9c7bd6b34))
* **helper:** serialize jail building with flock ([75a4569](https://github.com/first-it-consulting/harness-control-center/commit/75a4569b24da4744d3dcb99f8fa0dfc4f37655df))
* **network:** block host addresses, keep other machines off the proxy ([1e0528d](https://github.com/first-it-consulting/harness-control-center/commit/1e0528dee504f300b6683962404c884b15ce4c2d))
* **plugins:** check the kill switch under the dispatch lock ([f835553](https://github.com/first-it-consulting/harness-control-center/commit/f835553b1a718d9fdde5eb2959615c9192b14386))
* **plugins:** the kill switch stops plugins too ([7267a83](https://github.com/first-it-consulting/harness-control-center/commit/7267a83468a7d9f0c36d74e5cc66b92cc981d58d))
* **policy:** reject malformed egress hosts and package references ([0140300](https://github.com/first-it-consulting/harness-control-center/commit/01403008c5f0df02d4bc07526353693aa38c1346))
* **results:** overflow-safe limits, count directories, drop sparse files ([1ab67b0](https://github.com/first-it-consulting/harness-control-center/commit/1ab67b02a94b8b9117c168272afb19807c5c592a))
* **runner:** FIFO dispatch, target check, lost-cancel race ([1a39e99](https://github.com/first-it-consulting/harness-control-center/commit/1a39e99cf116e237c1841f1364ea492218216209))
* **sandbox:** confine images to ImageRoot and fix Windows tests ([06a90e2](https://github.com/first-it-consulting/harness-control-center/commit/06a90e2292e70b87dbcce5fcfdbf25c52fcc20c1))
* **sandbox:** set jail file modes explicitly, independent of umask ([b6f4f0d](https://github.com/first-it-consulting/harness-control-center/commit/b6f4f0d708ca173269183845ddea0695b5e746bc))
* **schedules:** check every run against the runner's current config ([ad2b184](https://github.com/first-it-consulting/harness-control-center/commit/ad2b184c17af2f987030aa7fb952d2473f8ee9a9))
* **security:** bound total jail space and clean up partial jails ([64fe685](https://github.com/first-it-consulting/harness-control-center/commit/64fe68564175bd52c4f7f2e5aed0ba9477a65b70))
* **security:** cap staged file sizes, fail closed on the chroot base ([f4f46ae](https://github.com/first-it-consulting/harness-control-center/commit/f4f46ae75e0683b4ae7edc1e954f5558bbaa8a07))
* **security:** per-VM identities and terminal-safe console output ([550ddb0](https://github.com/first-it-consulting/harness-control-center/commit/550ddb0bea92251b4f3cdaa0035f860e7fa5ec15))
* **security:** root-owned jails built by the helper (no TOCTOU) ([f51508c](https://github.com/first-it-consulting/harness-control-center/commit/f51508c769b9668cf097e522f90a70c327f0ea08))
