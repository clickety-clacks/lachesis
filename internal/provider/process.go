package provider

import (
	"context"
	"io"
	"os/exec"
)

type commandProcess struct {
	cmd    *exec.Cmd
	reader *io.PipeReader
	writer *io.PipeWriter
}

func StartCommand(ctx context.Context, executable string, args []string, env []string) (LoginProcess, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = env
	r, w := io.Pipe()
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	return &commandProcess{cmd: cmd, reader: r, writer: w}, nil
}
func (p *commandProcess) Output() io.ReadCloser { return p.reader }
func (p *commandProcess) Wait() error           { err := p.cmd.Wait(); p.writer.Close(); return err }
func (p *commandProcess) Terminate() error      { return p.cmd.Process.Signal(interruptSignal()) }
func (p *commandProcess) Kill() error           { return p.cmd.Process.Kill() }
