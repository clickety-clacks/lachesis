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
	stdin  io.WriteCloser
}

func StartCommand(ctx context.Context, executable string, args []string, env []string) (LoginProcess, error) {
	// The job manager owns the interrupt, grace, kill, and reap sequence. Binding
	// the command to ctx would let os/exec kill it before that owner can act.
	_ = ctx
	cmd := exec.Command(executable, args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, w := io.Pipe()
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		stdin.Close()
		r.Close()
		w.Close()
		return nil, err
	}
	return &commandProcess{cmd: cmd, reader: r, writer: w, stdin: stdin}, nil
}
func (p *commandProcess) Output() io.ReadCloser { return p.reader }
func (p *commandProcess) Wait() error           { err := p.cmd.Wait(); p.writer.Close(); return err }
func (p *commandProcess) Terminate() error      { return p.cmd.Process.Signal(interruptSignal()) }
func (p *commandProcess) Kill() error           { return p.cmd.Process.Kill() }
func (p *commandProcess) SubmitCode(code string) error {
	_, err := io.WriteString(p.stdin, code+"\n")
	return err
}
