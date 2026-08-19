package processcheck

import (
	"context"
	"os/exec"
	"strings"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type Checker interface {
	Busy(context.Context, model.Provider) (bool, error)
}

type PS struct{}

func (PS) Busy(ctx context.Context, p model.Provider) (bool, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "comm=").Output()
	if err != nil {
		return false, err
	}
	want := string(p)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		base := fields[0]
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if base == want {
			return true, nil
		}
	}
	return false, nil
}
