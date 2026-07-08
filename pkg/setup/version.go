package setup

// 버전 식별자 — 빌드 시 git tag 를 ldflags 로 주입한다 (build/build-*.bat):
//
//	go build -ldflags "-X github.com/isannai/isann-servers/pkg/setup.IsannCliVersion=v0.2.0" ...
//
// ⚠️ **반드시 var 여야 한다 (const 아님)**. 링커 `-X` 는 string *변수* 만
// 덮어쓴다 — const 는 컴파일 타임에 inline 돼서 주입이 조용히 무시된다
// (에러도 안 남). "정리"한답시고 const 로 되돌리지 말 것. 여기 "0.1.0" 은
// ldflags 없이 빌드할 때(로컬 go build)의 fallback 기본값.
// 정책: docs/TODO/pkg_policy/package-distribution-policy.md §5.2
var (
	StationVersion    = "0.1.0"
	ControlVersion    = "0.1.0"
	GateVersion       = "0.1.0"
	MarketVersion     = "0.1.0"
	RendezvousVersion = "0.1.0"
	FetcherVersion    = "0.1.0"
	IsanndVersion     = "0.1.0"
	IsannCliVersion   = "0.1.20"
	IvmVersion        = "0.1.0"
)

// Branding constants. ProjectName is the user-facing display form;
// FetcherBin is the worker binary basename (lowercase, no extension) that
// isannd spawns for model download / binary update / installer subprocess
// tasks. IvmBin is the standalone node lifecycle manager binary (install +
// version + init + repair) that lives at install-root and is the operator's
// single entry point for bootstrap (see docs/TODO/isann-ivm-design.md).
// Build scripts hardcode the matching string separately.
const (
	ProjectName = "iSANN"
	FetcherBin  = "isann-fetcher"
	IvmBin      = "ivm"
)
