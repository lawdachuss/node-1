package config

import "runtime"

// VM-sized concurrency pools.
//
// The fleet runs on GitHub-hosted runners, which come in FIXED VM sizes
// (standard windows-latest = 2 vCPU / 7 GB RAM; larger runners = 4/8/16/32/64
// vCPU).  Pools hardcoded for a 2-core box undershoot a larger runner, and
// oversized pools would thrash a 2-core one.  These functions derive every
// parallelism knob from the VM's vCPU count, anchored so 2 vCPU yields exactly
// the tuned production baseline (the upload "fast" tier — uploads are
// connection-bound, not CPU-bound, so the pools deliberately out-provision
// the host fleet's per-host rate-limit caps rather than the VM):
//
//	2 vCPU:  24 retry workers · 256 UploadSem · 24 GoFile / 16 other host slots · 6 pipelines/channel
//	4 vCPU:  48               · 512           · 48        / 32                · 12
//	8 vCPU:  64 (cap)         · 1024 (cap)    · 48 (cap)  / 32 (cap)          · 24
//	16 vCPU: 64               · 1024          · 48        / 32                · 32 (cap)
//
// The retry-worker pool must be at least as large as the per-host slot caps so
// enough files are in flight simultaneously to actually fill every host's
// concurrency slots.  Host-facing caps stay bounded: they reflect the upload
// hosts' rate-limit tolerance (429 backoff), not VM resources, so they never
// exceed sane ceilings even on huge runners.
const (
	minRetryWorkers      = 16
	maxRetryWorkers      = 64
	minUploadSem         = 192
	maxUploadSem         = 1024
	minGoFileConcurrency = 16
	maxGoFileConcurrency = 48
	minOtherConcurrency  = 12
	maxOtherConcurrency  = 32
	minPipelineWorkers   = 4
	maxPipelineWorkers   = 32
)

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// sizedFor computes the concurrency pools for a VM with n vCPUs.  Exposed for
// tests; VMSizedConcurrency calls it with the real CPU count.
func sizedFor(n int) (retryWorkers, uploadSem, goFile, other, pipelineWorkers int) {
	retryWorkers = clampInt(n*12, minRetryWorkers, maxRetryWorkers)
	uploadSem = clampInt(n*128, minUploadSem, maxUploadSem)
	goFile = clampInt(n*12, minGoFileConcurrency, maxGoFileConcurrency)
	other = clampInt(n*8, minOtherConcurrency, maxOtherConcurrency)
	pipelineWorkers = clampInt(n*3, minPipelineWorkers, maxPipelineWorkers)
	return
}

// VMSizedConcurrency returns the parallelism pools for the current VM,
// derived from runtime.NumCPU(): retry workers, UploadSem, GoFile per-host
// cap, other-hosts per-host cap, and pipelines per channel queue.
func VMSizedConcurrency() (retryWorkers, uploadSem, goFile, other, pipelineWorkers int) {
	return sizedFor(runtime.NumCPU())
}
