//go:build !windows

package secretstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type osStore struct {
	namespace, command string
	mac                bool
}

func NewOS(namespace string) (facade.SecretStore, error) {
	name := "secret-tool"
	if runtime.GOOS == "darwin" {
		name = "security"
	} else if runtime.GOOS != "linux" {
		return nil, ErrUnavailable
	}
	command, err := exec.LookPath(name)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &osStore{namespace: base64.RawURLEncoding.EncodeToString([]byte(namespace)), command: command, mac: runtime.GOOS == "darwin"}, nil
}
func (s *osStore) run(ctx context.Context, input string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.command, args...)
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ((s.mac && exit.ExitCode() == 44) || (!s.mac && exit.ExitCode() == 1 && stderr.Len() == 0)) {
			return nil, ErrNotFound
		}
		return nil, ErrUnavailable
	}
	if s.mac && len(args) == 1 && args[0] == "-i" && stderr.Len() > 0 {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (s *osStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	key = base64.RawURLEncoding.EncodeToString([]byte(key))
	args := []string{"lookup", "namespace", s.namespace, "key", key}
	if s.mac {
		args = []string{"find-generic-password", "-s", s.namespace, "-a", key, "-w"}
	}
	out, err := s.run(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	value, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, errors.New("invalid OS secret encoding")
	}
	return value, nil
}
func (s *osStore) Set(ctx context.Context, key string, value []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	key = base64.RawURLEncoding.EncodeToString([]byte(key))
	encoded := base64.StdEncoding.EncodeToString(value)
	var err error
	if s.mac {
		// Base64 confines interactive arguments to literal data; secrets never enter process argv.
		_, err = s.run(ctx, "add-generic-password -U -s "+s.namespace+" -a "+key+" -w \""+encoded+"\"\n", "-i")
	} else {
		_, err = s.run(ctx, encoded+"\n", "store", "--label=Omni secret", "namespace", s.namespace, "key", key)
	}
	return err
}
func (s *osStore) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	key = base64.RawURLEncoding.EncodeToString([]byte(key))
	args := []string{"clear", "namespace", s.namespace, "key", key}
	if s.mac {
		args = []string{"delete-generic-password", "-s", s.namespace, "-a", key}
	}
	_, err := s.run(ctx, "", args...)
	return err
}
