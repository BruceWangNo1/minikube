/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package command

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sync"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/util"
)

// SSHRunner runs commands through SSH.
//
// It implements the CommandRunner interface.
type SSHRunner struct {
	c *ssh.Client
}

// NewSSHRunner returns a new SSHRunner that will run commands
// through the ssh.Client provided.
func NewSSHRunner(c *ssh.Client) *SSHRunner {
	return &SSHRunner{c}
}

// Remove runs a command to delete a file on the remote.
func (s *SSHRunner) Remove(f assets.CopyableFile) error {
	sess, err := s.c.NewSession()
	if err != nil {
		return errors.Wrap(err, "getting ssh session")
	}
	defer sess.Close()
	cmd := getDeleteFileCommand(f)
	return sess.Run(cmd)
}

type singleWriter struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (w *singleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// teeSSH runs an SSH command, streaming stdout, stderr to logs
func teeSSH(s *ssh.Session, cmd string, outB io.Writer, errB io.Writer) error {
	outPipe, err := s.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "stdout")
	}

	errPipe, err := s.StderrPipe()
	if err != nil {
		return errors.Wrap(err, "stderr")
	}
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		if err := util.TeePrefix(util.ErrPrefix, errPipe, errB, glog.Infof); err != nil {
			glog.Errorf("tee stderr: %v", err)
		}
		wg.Done()
	}()
	go func() {
		if err := util.TeePrefix(util.OutPrefix, outPipe, outB, glog.Infof); err != nil {
			glog.Errorf("tee stdout: %v", err)
		}
		wg.Done()
	}()
	err = s.Run(cmd)
	wg.Wait()
	return err
}

// Run starts a command on the remote and waits for it to return.
func (s *SSHRunner) Run(cmd string) error {
	glog.Infof("SSH: %s", cmd)
	sess, err := s.c.NewSession()
	if err != nil {
		return errors.Wrap(err, "NewSession")
	}

	defer func() {
		if err := sess.Close(); err != nil {
			if err != io.EOF {
				glog.Errorf("session close: %v", err)
			}
		}
	}()
	var outB bytes.Buffer
	var errB bytes.Buffer
	err = teeSSH(sess, cmd, &outB, &errB)
	if err != nil {
		return errors.Wrapf(err, "command failed: %s\nstdout: %s\nstderr: %s", cmd, outB.String(), errB.String())
	}
	return nil
}

// CombinedOutputTo runs the command and stores both command
// output and error to out.
func (s *SSHRunner) CombinedOutputTo(cmd string, w io.Writer) error {
	out, err := s.CombinedOutput(cmd)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(out))
	return err
}

// CombinedOutput runs the command on the remote and returns its combined
// standard output and standard error.
func (s *SSHRunner) CombinedOutput(cmd string) (string, error) {
	glog.Infoln("Run with output:", cmd)
	sess, err := s.c.NewSession()
	if err != nil {
		return "", errors.Wrap(err, "NewSession")
	}
	defer sess.Close()

	var combined singleWriter
	err = teeSSH(sess, cmd, &combined, &combined)
	out := combined.b.String()
	if err != nil {
		return out, err
	}
	return out, nil
}

// Copy copies a file to the remote over SSH.
func (s *SSHRunner) Copy(f assets.CopyableFile) error {
	sess, err := s.c.NewSession()
	if err != nil {
		return errors.Wrap(err, "NewSession")
	}

	w, err := sess.StdinPipe()
	if err != nil {
		return errors.Wrap(err, "StdinPipe")
	}
	// The scpcmd below *should not* return until all data is copied and the
	// StdinPipe is closed. But let's use errgroup to make it explicit.
	var g errgroup.Group
	var copied int64
	dst := filepath.Join(path.Join(f.GetTargetDir(), f.GetTargetName()))
	glog.Infof("Transferring %d bytes to %s", f.GetLength(), dst)

	g.Go(func() error {
		defer w.Close()
		header := fmt.Sprintf("C%s %d %s\n", f.GetPermissions(), f.GetLength(), f.GetTargetName())
		fmt.Fprint(w, header)
		if f.GetLength() == 0 {
			glog.Warningf("%s is a 0 byte asset!", f.GetTargetName())
			fmt.Fprint(w, "\x00")
			return nil
		}

		copied, err = io.Copy(w, f)
		if err != nil {
			return errors.Wrap(err, "io.Copy")
		}
		if copied != int64(f.GetLength()) {
			return fmt.Errorf("%s: expected to copy %d bytes, but copied %d instead", f.GetTargetName(), f.GetLength(), copied)
		}
		glog.Infof("%s: copied %d bytes", f.GetTargetName(), copied)
		fmt.Fprint(w, "\x00")
		return nil
	})

	scp := fmt.Sprintf("sudo mkdir -p %s && sudo scp -t %s", f.GetTargetDir(), f.GetTargetDir())
	out, err := sess.CombinedOutput(scp)
	if err != nil {
		return fmt.Errorf("%s: %s\noutput: %s", scp, err, out)
	}
	return g.Wait()
}
