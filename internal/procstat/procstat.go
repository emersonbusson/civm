// Package procstat le o campo starttime (field 22) de /proc/<pid>/stat — os
// "start ticks" do processo: o numero de clock ticks desde o boot em que ele
// nasceu. Esse valor e o discriminador de PID-reuse: combinado com kill -0
// (PID ainda vivo), o starttime distingue o holder original de um PID reciclado
// pelo kernel para outro processo. Tanto admit quanto dockerlock dependiam de
// copias identicas deste parser; este pacote e a fonte unica compartilhada.
package procstat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStatPath e o template do caminho do arquivo de stat de um PID no procfs.
const procStatPath = "/proc/%d/stat"

// procStatStartTimeFieldIndex e o indice (base zero) do campo starttime dentro
// dos campos que vem DEPOIS do comm. O comm e o field 2 do /proc/<pid>/stat;
// apos ele, o primeiro campo (rest[0]) e o field 3 (state). O starttime e o
// field 22, logo rest[22-3] = rest[19].
const procStatStartTimeFieldIndex = 19

// PidStartTicks le o field 22 (starttime, clock ticks desde o boot) de
// /proc/<pid>/stat. O campo comm (field 2) vem entre parenteses e pode conter
// espacos e ')', entao o parse dos campos seguintes so comeca apos o ultimo ')'.
func PidStartTicks(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf(procStatPath, pid))
	if err != nil {
		return 0, fmt.Errorf("ler /proc/%d/stat: %w", pid, err)
	}
	return parseStartTicks(string(data))
}

// parseStartTicks extrai o field 22 (starttime) de uma linha de
// /proc/<pid>/stat. O field 2 (comm) e embrulhado em parenteses e pode conter
// espacos e ')', por isso os campos restantes so sao lidos apos o ultimo ')'.
func parseStartTicks(stat string) (uint64, error) {
	commEnd := strings.LastIndexByte(stat, ')')
	if commEnd < 0 || commEnd+2 > len(stat) {
		return 0, fmt.Errorf("stat sem campo comm: %q", truncateStat(stat))
	}
	rest := strings.Fields(stat[commEnd+1:])
	if len(rest) <= procStatStartTimeFieldIndex {
		return 0, fmt.Errorf("stat com poucos campos (%d)", len(rest))
	}
	ticks, err := strconv.ParseUint(rest[procStatStartTimeFieldIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime %q: %w", rest[procStatStartTimeFieldIndex], err)
	}
	return ticks, nil
}

// truncateStat corta a linha de stat a 64 bytes para mensagens de erro — evita
// despejar um /proc/<pid>/stat inteiro (potencialmente longo) no log de erro.
func truncateStat(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}
