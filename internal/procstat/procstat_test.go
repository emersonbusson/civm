package procstat

import (
	"os"
	"strings"
	"testing"
)

// TestParseStartTicks exercita o parser puro de uma linha de /proc/<pid>/stat:
// comm simples, comm com espacos e parenteses (o caso que quebra um split
// ingenuo por espaco), e os tres modos de erro (sem ')', poucos campos,
// starttime nao-numerico). Portado de internal/dockerlock para preservar a
// cobertura quando o parser foi extraido para este pacote compartilhado.
func TestParseStartTicks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		stat    string
		want    uint64
		wantErr bool
	}{
		{
			name: "simple comm",
			// fields: 1=pid 2=(comm) 3=state ... 22=starttime
			stat: "1234 (bash) S 1 1234 1234 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 9876543 0 0",
			want: 9876543,
		},
		{
			name: "comm with spaces and parens",
			stat: "5678 (My Proc (x)) R 1 5678 5678 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0",
			want: 4242,
		},
		{
			name:    "missing comm close paren",
			stat:    "1234 bash S 1",
			wantErr: true,
		},
		{
			name:    "too few fields after comm",
			stat:    "1234 (bash) S 1 1",
			wantErr: true,
		},
		{
			name:    "non-numeric starttime",
			stat:    "1234 (bash) S 1 1234 1234 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 notanumber 0 0",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseStartTicks(c.stat)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseStartTicks(%q) err = nil, want error", c.stat)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStartTicks(%q) err = %v", c.stat, err)
			}
			if got != c.want {
				t.Fatalf("parseStartTicks(%q) = %d, want %d", c.stat, got, c.want)
			}
		})
	}
}

// TestPidStartTicks le o field 22 do /proc real para o nosso proprio PID e
// confirma que uma entrada de /proc inexistente vira erro (caminho de read
// error). Portado de dockerlock's TestDefaultPidStartTicks.
func TestPidStartTicks(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc on this platform")
	}
	ticks, err := PidStartTicks(os.Getpid())
	if err != nil {
		t.Fatalf("PidStartTicks(self) err = %v", err)
	}
	if ticks == 0 {
		t.Fatalf("PidStartTicks(self) = 0, want non-zero starttime")
	}
	// Um PID que nao pode existir forca o caminho de erro de leitura do /proc.
	if _, err := PidStartTicks(1 << 30); err == nil {
		t.Fatalf("PidStartTicks(huge) err = nil, want error")
	}
}

// TestTruncateStat cobre o pass-through curto e o corte em 64 bytes (usado nas
// mensagens de erro para nao despejar uma linha de stat inteira).
func TestTruncateStat(t *testing.T) {
	t.Parallel()
	short := "1234 (bash) S"
	if got := truncateStat(short); got != short {
		t.Fatalf("truncateStat(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 100)
	got := truncateStat(long)
	if len(got) != 64 {
		t.Fatalf("truncateStat(long) len = %d, want 64", len(got))
	}
}
