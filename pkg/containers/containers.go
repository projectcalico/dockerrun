// Copyright (c) 2017-2018 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package containers

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/dockerrun/pkg/set"
	"github.com/projectcalico/dockerrun/pkg/utils"
)

type Container struct {
	Name     string
	IP       string
	Hostname string
	runCmd   *exec.Cmd

	mutex         sync.Mutex
	binaries      set.Set
	stdoutWatches []*watch

	logFinished sync.WaitGroup
}

type watch struct {
	regexp *regexp.Regexp
	c      chan struct{}
}

var containerIdx = 0

func (c *Container) Stop() error {
	if c == nil {
		log.Info("Stop no-op because nil container")
		return nil
	}

	logCxt := log.WithField("container", c.Name)
	c.mutex.Lock()
	if c.runCmd == nil {
		logCxt.Info("Stop no-op because container is not running")
		c.mutex.Unlock()
		return nil
	}
	c.mutex.Unlock()

	logCxt.Info("Stop")

	// Ask docker to stop the container.
	withTimeoutPanic(logCxt, 30*time.Second, c.execDockerStop)
	// Shut down the docker run process (if needed).
	withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Interrupt) })

	// Wait for the container to exit, then escalate to killing it.
	startTime := time.Now()
	for {
		isRunning, err := c.ListedInDockerPS()
		if err != nil {
			return err
		}
		if !isRunning {
			// Container has stopped.  Mkae sure the docker CLI command is dead (it should be already)
			// and wait for its log.
			logCxt.Info("Container stopped (no longer listed in 'docker ps')")
			withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Kill) })
			withTimeoutPanic(logCxt, 10*time.Second, func() { c.logFinished.Wait() })
			return nil
		}
		if time.Since(startTime) > 2*time.Second {
			logCxt.Info("Container didn't stop, asking docker to kill it")
			// `docker kill` asks the docker daemon to kill the container but, on a
			// resource constrained system, we've seen that fail because the CLI command
			// was blocked so we kill the CLI command too.
			err := exec.Command("docker", "kill", c.Name).Run()
			logCxt.WithError(err).Info("Ran 'docker kill'")
			withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Kill) })
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.WaitNotRunning(60 * time.Second)
	logCxt.Info("Container stopped")
	withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Kill) })
	withTimeoutPanic(logCxt, 10*time.Second, func() { c.logFinished.Wait() })

	return nil
}

func withTimeoutPanic(logCxt *log.Entry, t time.Duration, f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()

	select {
	case <-done:
		return
	case <-time.After(t):
		logCxt.Panic("Timeout!")
	}
}

func (c *Container) execDockerStop() {
	logCxt := log.WithField("container", c.Name)
	logCxt.Info("Executing 'docker stop'")
	cmd := exec.Command("docker", "stop", c.Name)
	err := cmd.Run()
	if err != nil {
		logCxt.WithError(err).WithField("cmd", cmd).Error("docker stop command failed")
		return
	}
	logCxt.Info("'docker stop' returned success")
}

func (c *Container) signalDockerRun(sig os.Signal) {
	logCxt := log.WithFields(log.Fields{
		"container": c.Name,
		"signal":    sig,
	})
	logCxt.Info("Sending signal to 'docker run' process")
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.runCmd == nil {
		return
	}
	c.runCmd.Process.Signal(sig)
	logCxt.Info("Signalled docker run")
}

type RunOpts struct {
	AutoRemove bool
}

func Run(namePrefix string, opts RunOpts, args ...string) (c *Container, err error) {

	// Build unique container name and struct.
	containerIdx++
	c = &Container{Name: fmt.Sprintf("%v-%d-%d-felixfv", namePrefix, os.Getpid(), containerIdx)}

	// Prep command to run the container.
	log.WithField("container", c).Info("About to run container")
	runArgs := []string{"run", "--name", c.Name, "--hostname", c.Name}

	if opts.AutoRemove {
		runArgs = append(runArgs, "--rm")
	}

	// Add remaining args
	runArgs = append(runArgs, args...)

	c.runCmd = utils.Command("docker", runArgs...)

	// Get the command's output pipes, so we can merge those into the test's own logging.
	stdout, err := c.runCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %v", err)
	}
	stderr, err := c.runCmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stderr pipe: %v", err)
	}

	// Start the container running.
	if err := c.runCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to run container start cmd: %v", err)
	}

	// Merge container's output into our own logging.
	c.logFinished.Add(2)
	go c.copyOutputToLog("stdout", stdout, &c.logFinished, &c.stdoutWatches)
	go c.copyOutputToLog("stderr", stderr, &c.logFinished, nil)

	// Note: it might take a long time for the container to start running, e.g. if the image
	// needs to be downloaded.
	if err := c.WaitUntilRunning(); err != nil {
		return nil, fmt.Errorf("container failed to run: %v", err)
	}

	// Fill in rest of container struct.
	c.IP, err = c.GetIP()
	if err != nil {
		return nil, err
	}
	c.Hostname, err = c.GetHostname()
	if err != nil {
		return nil, err
	}

	c.binaries = set.New()
	log.WithField("container", c).Info("Container now running")
	return
}

func (c *Container) WatchStdoutFor(re *regexp.Regexp) chan struct{} {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	log.WithFields(log.Fields{
		"container": c.Name,
		"regex":     re,
	}).Info("Start watching stdout")

	ch := make(chan struct{})
	c.stdoutWatches = append(c.stdoutWatches, &watch{
		regexp: re,
		c:      ch,
	})
	return ch
}

// Start executes "docker start" on a container. Useful when used after Stop()
// to restart a container.
func (c *Container) Start() error {
	c.runCmd = utils.Command("docker", "start", "--attach", c.Name)

	stdout, err := c.runCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %v", err)
	}
	stderr, err := c.runCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %v", err)
	}

	// Start the container running.
	if err := c.runCmd.Start(); err != nil {
		return err
	}

	// Merge container's output into our own logging.
	c.logFinished.Add(2)
	go c.copyOutputToLog("stdout", stdout, &c.logFinished, &c.stdoutWatches)
	go c.copyOutputToLog("stderr", stderr, &c.logFinished, nil)

	if err := c.WaitUntilRunning(); err != nil {
		return err
	}

	log.WithField("container", c).Info("Container now running")

	return nil
}

// Remove deletes a container. Should be manually called after a non-auto-removed container
// is stopped.
func (c *Container) Remove() error {
	c.runCmd = utils.Command("docker", "rm", "-f", c.Name)
	if err := c.runCmd.Start(); err != nil {
		return err
	}

	log.WithField("container", c).Info("Removed container.")
	return nil
}

func (c *Container) copyOutputToLog(streamName string, stream io.Reader, done *sync.WaitGroup, watches *[]*watch) {
	defer done.Done()
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(nil, 10*1024*1024) // Increase maximum buffer size (but don't pre-alloc).
	for scanner.Scan() {
		line := scanner.Text()
		log.Info(c.Name, "[", streamName, "] ", line)

		if watches == nil {
			continue
		}
		c.mutex.Lock()
		for _, w := range *watches {
			if w.c == nil {
				continue
			}
			if !w.regexp.MatchString(line) {
				continue
			}

			log.Info(c.Name, "[", streamName, "] ", "Watch triggered:", w.regexp.String())
			close(w.c)
			w.c = nil
		}
		c.mutex.Unlock()
	}
	logCxt := log.WithFields(log.Fields{
		"name":   c.Name,
		"stream": stream,
	})
	if scanner.Err() != nil {
		logCxt.WithError(scanner.Err()).Error("Non-EOF error reading container stream")
	}
	logCxt.Info("Stream finished")
}

func (c *Container) DockerInspect(format string) (string, error) {
	inspectCmd := utils.Command("docker", "inspect",
		"--format="+format,
		c.Name,
	)
	outputBytes, err := inspectCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run docker inspect: %v", err)
	}
	return string(outputBytes), nil
}

func (c *Container) GetIP() (string, error) {
	output, err := c.DockerInspect("{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")
	return strings.TrimSpace(output), err
}

func (c *Container) GetHostname() (string, error) {
	output, err := c.DockerInspect("{{.Config.Hostname}}")
	return strings.TrimSpace(output), err
}

func (c *Container) GetPIDs(processName string) []int {
	out, err := c.ExecOutput("pgrep", fmt.Sprintf("^%s$", processName))
	if err != nil {
		log.WithError(err).Warn("pgrep failed, assuming no PIDs")
		return nil
	}
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			panic(fmt.Sprintf("failed to convert pid '%s' to int. Parsing error?", line))
		}
		pids = append(pids, pid)
	}
	return pids
}

func (c *Container) GetSinglePID(processName string) (int, error) {
	// Get the process's PID.  This retry loop ensures that we don't get tripped up if we see multiple
	// PIDs, which can happen transiently when a process restarts/forks off a subprocess.
	start := time.Now()
	for {
		pids := c.GetPIDs(processName)
		if len(pids) == 1 {
			return pids[0], nil
		}

		if time.Since(start) < time.Second {
			return 0, errors.New("Timed out waiting for there to be a single PID")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *Container) WaitUntilRunning() error {
	log.Info("Wait for container to be listed in docker ps")

	// Set up so we detect if container startup fails.
	stoppedChan := make(chan error)
	go func() {
		defer close(stoppedChan)
		stoppedChan <- c.runCmd.Wait()

		// log.WithError(err).WithField("name", c.Name).Info("Container stopped ('docker run' exited)")
		c.mutex.Lock()
		defer c.mutex.Unlock()
		c.runCmd = nil
	}()

	tickChan := time.NewTicker(time.Second).C

	for {
		select {
		case <-tickChan:
			cmd := utils.Command("docker", "ps")
			out, err := cmd.CombinedOutput()
			if err != nil {
				return err
			}
			if strings.Contains(string(out), c.Name) {
				// success - we found the container in 'docker ps' output.
				return nil
			}
		case err := <-stoppedChan:
			// the process stopped. whether or not it failed, we return an error.
			return fmt.Errorf("Container stopped before being listed in 'docker ps': %v", err)
		}
	}
}

func (c *Container) Stopped() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.runCmd == nil
}

func (c *Container) ListedInDockerPS() (bool, error) {
	cmd := utils.Command("docker", "ps")
	out, err := cmd.CombinedOutput()
	return strings.Contains(string(out), c.Name), err
}

func (c *Container) WaitNotRunning(timeout time.Duration) error {
	log.Info("Wait for container not to be listed in docker ps")
	start := time.Now()
	for {
		isRunning, err := c.ListedInDockerPS()
		if err != nil {
			return err
		}
		if !isRunning {
			break
		}
		if time.Since(start) > timeout {
			log.Panic("Timed out waiting for container not to be listed.")
		}
		time.Sleep(1000 * time.Millisecond)
	}

	return nil
}

func (c *Container) EnsureBinary(name string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.binaries.Contains(name) {
		utils.Command("docker", "cp", "../bin/"+name, c.Name+":/"+name).Run()
		c.binaries.Add(name)
	}
}

func (c *Container) CopyFileIntoContainer(hostPath, containerPath string) error {
	cmd := utils.Command("docker", "cp", hostPath, c.Name+":"+containerPath)
	return cmd.Run()
}

func (c *Container) Exec(cmd ...string) error {
	log.WithField("container", c.Name).WithField("command", cmd).Info("Running command")
	arg := []string{"exec", c.Name}
	arg = append(arg, cmd...)
	return utils.Run("docker", arg...)
}

func (c *Container) ExecOutput(args ...string) (string, error) {
	arg := []string{"exec", c.Name}
	arg = append(arg, args...)
	cmd := exec.Command("docker", arg...)
	out, err := cmd.Output()
	if err != nil {
		if out == nil {
			return "", err
		}
		return string(out), err
	}
	return string(out), nil
}

func (c *Container) SourceName() string {
	return c.Name
}

func (c *Container) CanConnectTo(ip, port, protocol string) (bool, error) {

	// Ensure that the container has the 'test-connection' binary.
	c.EnsureBinary("test-connection")

	// Run 'test-connection' to the target.
	connectionCmd := utils.Command("docker", "exec", c.Name,
		"/test-connection", "--protocol="+protocol, "-", ip, port)
	outPipe, err := connectionCmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	errPipe, err := connectionCmd.StderrPipe()
	if err != nil {
		return false, err
	}
	err = connectionCmd.Start()
	if err != nil {
		return false, err
	}

	wOut, err := ioutil.ReadAll(outPipe)
	if err != nil {
		return false, err
	}
	wErr, err := ioutil.ReadAll(errPipe)
	if err != nil {
		return false, err
	}
	err = connectionCmd.Wait()

	log.WithFields(log.Fields{
		"stdout": string(wOut),
		"stderr": string(wErr)}).WithError(err).Info("Connection test")

	return false, nil
}
