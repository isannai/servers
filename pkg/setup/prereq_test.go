package setup

import (
	"reflect"
	"testing"
)

// TestNewPrereqStatus_DockerReady — DockerReady 는 Docker component 가 OK 일
// 때만 true (exit-code 0=준비됨 계약). 다른 component OK 여도 docker 가 아니면 false.
func TestNewPrereqStatus_DockerReady(t *testing.T) {
	cases := []struct {
		name      string
		comps     []PrereqComponent
		wantReady bool
	}{
		{
			name: "docker ok -> ready",
			comps: []PrereqComponent{
				{Name: PrereqWSL2, Status: PrereqOK, AutoFix: true},
				{Name: PrereqDocker, Status: PrereqOK, AutoFix: true},
			},
			wantReady: true,
		},
		{
			name: "docker missing -> not ready",
			comps: []PrereqComponent{
				{Name: PrereqWSL2, Status: PrereqOK, AutoFix: true},
				{Name: PrereqDocker, Status: PrereqMissing, AutoFix: true},
			},
			wantReady: false,
		},
		{
			name: "docker unknown (WSL stopped) -> not ready",
			comps: []PrereqComponent{
				{Name: PrereqWSL2, Status: PrereqOK, AutoFix: true},
				{Name: PrereqDocker, Status: PrereqUnknown, AutoFix: true},
			},
			wantReady: false,
		},
		{
			name: "everything-but-docker ok -> not ready",
			comps: []PrereqComponent{
				{Name: PrereqWSL2, Status: PrereqOK, AutoFix: true},
				{Name: PrereqDriver, Status: PrereqOK, AutoFix: false},
			},
			wantReady: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newPrereqStatus("windows", tc.comps)
			if st.DockerReady != tc.wantReady {
				t.Fatalf("DockerReady = %v, want %v", st.DockerReady, tc.wantReady)
			}
			if st.OS != "windows" {
				t.Fatalf("OS = %q, want windows", st.OS)
			}
		})
	}
}

// TestMissingAutoFix — `ivm setup` 이 채울 수 있는 빠진 항목만 반환.
// 검출-전용(AutoFix=false, 드라이버 등)은 빠져도 제외. unknown/na 도 제외.
func TestMissingAutoFix(t *testing.T) {
	st := newPrereqStatus("windows", []PrereqComponent{
		{Name: PrereqWSL2, Status: PrereqOK, AutoFix: true},              // ok → 제외
		{Name: PrereqDistro, Status: PrereqMissing, AutoFix: true},       // ← 포함
		{Name: PrereqDocker, Status: PrereqUnknown, AutoFix: true},       // unknown → 제외
		{Name: PrereqToolkit, Status: PrereqMissing, AutoFix: true},      // ← 포함
		{Name: PrereqDriver, Status: PrereqMissing, AutoFix: false},      // 검출전용 → 제외
	})
	got := st.MissingAutoFix()
	want := []string{PrereqDistro, PrereqToolkit}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingAutoFix() = %v, want %v", got, want)
	}
}

// TestWSLDistro — distro component(OK)의 Detail 첫 항목을 distro 이름으로.
func TestWSLDistro(t *testing.T) {
	st := newPrereqStatus("windows", []PrereqComponent{
		{Name: PrereqDistro, Status: PrereqOK, Detail: "Ubuntu-20.04, Debian"},
	})
	if got := st.WSLDistro(); got != "Ubuntu-20.04" {
		t.Fatalf("WSLDistro() = %q, want Ubuntu-20.04", got)
	}

	// 단일 distro
	st1 := newPrereqStatus("windows", []PrereqComponent{
		{Name: PrereqDistro, Status: PrereqOK, Detail: "Ubuntu-22.04"},
	})
	if got := st1.WSLDistro(); got != "Ubuntu-22.04" {
		t.Fatalf("WSLDistro() = %q, want Ubuntu-22.04", got)
	}

	// 미설치(OK 아님) → "" (검출-안내 메시지가 distro 이름으로 새지 않게)
	stMissing := newPrereqStatus("windows", []PrereqComponent{
		{Name: PrereqDistro, Status: PrereqMissing, Detail: "Ubuntu-22.04 not installed (...)"},
	})
	if got := stMissing.WSLDistro(); got != "" {
		t.Fatalf("WSLDistro() = %q, want empty for missing", got)
	}
}

// TestMissingAutoFix_AllReady — 빠진 자동설치 항목 없으면 nil/빈 슬라이스.
func TestMissingAutoFix_AllReady(t *testing.T) {
	st := newPrereqStatus("linux", []PrereqComponent{
		{Name: PrereqDocker, Status: PrereqOK, AutoFix: true},
		{Name: PrereqToolkit, Status: PrereqOK, AutoFix: true},
		{Name: PrereqDriver, Status: PrereqMissing, AutoFix: false}, // 검출전용 — setup 무관
	})
	if got := st.MissingAutoFix(); len(got) != 0 {
		t.Fatalf("MissingAutoFix() = %v, want empty", got)
	}
}
