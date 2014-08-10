package main

import (
	"errors"
	"fmt"
	"github.com/coreos/go-namespaces/namespace"
	"github.com/docker/libcontainer/security/capabilities"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
)

func nsenterdetect() (found bool, err error) {
	// We've inlined the subset of nsenter code we need for amd64 :)
	return true, nil
}

// from /usr/include/linux/sched.h
const (
	CLONE_VFORK = 0x00004000 /* set if the parent wants the child to wake it up on mm_release */
	SIGCHLD     = 0x14       /* Should set SIGCHLD for fork()-like behavior on Linux */
)

var cGroups = []string{ // FIXME - We should discover this dynamically
	"blkio",
	"cpuacct",
	"cpu",
	"devices",
	"freezer",
	"memory",
	"perf_event",
}

func addProcessToCgroupsForContainer(containerSha string, pid int) (err error) {
	for _, element := range cGroups {
		err = addProcessToCgroupForContainer(containerSha, element, pid)
		if err != nil {
			return err
		}
	}
	return nil
}

func addProcessToCgroupForContainer(containerSha string, group string, pid int) (err error) {
	path := fmt.Sprintf("/sys/fs/cgroup/%s/docker/%s/tasks", group, containerSha)
	f, err := os.Create(path)
	if err != nil {
		panic(fmt.Sprintf("Could not open %s: %s", path, err.Error()))
	}
	_, err = f.WriteString(fmt.Sprintf("%s\n", strconv.Itoa(pid)))
	if err != nil {
		panic("Could not write PID")
	}
	f.Close()
	return nil
}

func doChrootChwd(rootfd *os.File, cwdfd *os.File) (err error) {
	_, _, echrootdir := syscall.Syscall(syscall.SYS_FCHDIR, rootfd.Fd(), 0, 0)
	if echrootdir != 0 {
		panic("chdir to new root failed")
	}
	chrooterr := syscall.Chroot(".")
	if chrooterr != nil {
		panic(fmt.Sprintf("chroot failed: %s", chrooterr))
	}
	// FIXME - this cwds to the cwd of the 'root' process inside the container, we probably want to cwd to user's homedir instead?
	_, _, ecwd := syscall.Syscall(syscall.SYS_FCHDIR, cwdfd.Fd(), 0, 0)
	if ecwd != 0 {
		panic("cwd to working directory failed")
	}
	return nil
}

func nsenterexec(containerName string, uid int, gid int, wd string, shell string) (err error) {
	pid, err := dockerpid(containerName)
	if err != nil {
		panic(fmt.Sprintf("Could not get PID for container: %s", containerName))
	}
	containerSha, err := dockersha(containerName)
	if err != nil {
		panic(fmt.Sprintf("Could not get SHA for container: %s %s", err.Error(), containerName))
	}

	rootfd, rooterr := os.Open(fmt.Sprintf("/proc/%s/root", strconv.Itoa(pid)))
	if rooterr != nil {
		panic(fmt.Sprintf("Could not open fd to root: %s", rooterr))
	}
	// Find the user's homw directory (which should be bound in as a volume) in the
	// container process namespace, so we can chdir there later.
	cwdfd, cwderr := os.Open(fmt.Sprintf("/proc/%s/root%s", strconv.Itoa(pid), wd))
	if cwderr != nil {
		return errors.New(fmt.Sprintf("Could not open fd to desired cwd: %s", wd))
	}
	if strings.HasPrefix(shell, "/") != true {
		return errors.New(fmt.Sprintf("Shell '%s' does not start with /, need an absolute path", shell))
	}
	shell = path.Clean(shell)
	_, shellerr := os.Open(fmt.Sprintf("/proc/%s/root%s", strconv.Itoa(pid), shell))
	if shellerr != nil {
		return errors.New(fmt.Sprintf("Cannot find ynur shell %s inside your container", shell))
	}

	/* FIXME: Make these an array and loop through them, as this is gross */

	/* --ipc */
	ipcfd, ipcerr := namespace.OpenProcess(pid, namespace.CLONE_NEWIPC)
	if ipcfd == 0 || ipcerr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWIPC)")
	}

	/* --uts */
	utsfd, utserr := namespace.OpenProcess(pid, namespace.CLONE_NEWUTS)
	if utsfd == 0 || utserr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWUTS)")
	}

	/* --net */
	netfd, neterr := namespace.OpenProcess(pid, namespace.CLONE_NEWNET)
	if netfd == 0 || neterr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWNET)")
	}

	/* --pid */
	pidfd, piderr := namespace.OpenProcess(pid, namespace.CLONE_NEWPID)
	if pidfd == 0 || piderr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWPID)")
	}

	/* --mount */
	mountfd, mounterr := namespace.OpenProcess(pid, namespace.CLONE_NEWNS)
	if mountfd == 0 || mounterr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWNS)")
	}

	namespace.Setns(ipcfd, namespace.CLONE_NEWIPC)
	namespace.Setns(utsfd, namespace.CLONE_NEWUTS)
	namespace.Setns(netfd, namespace.CLONE_NEWNET)
	namespace.Setns(pidfd, namespace.CLONE_NEWPID)
	namespace.Setns(mountfd, namespace.CLONE_NEWNS)

	namespace.Close(ipcfd)
	namespace.Close(utsfd)
	namespace.Close(netfd)
	namespace.Close(pidfd)
	namespace.Close(mountfd)

	/* END FIXME */

	// see go/src/pkg/syscall/exec_unix.go - not sure if this is needed or not (or if we should lock a larger section)
	syscall.ForkLock.Lock()

	/* Stolen from https://github.com/tobert/lnxns/blob/master/src/lnxns/nsfork_linux.go
	   CLONE_VFORK implies that the parent process won't resume until the child calls Exec,
	   which fixes the potential race conditions */

	var flags int = SIGCHLD | CLONE_VFORK
	r1, _, err1 := syscall.RawSyscall(syscall.SYS_CLONE, uintptr(flags), 0, 0)

	syscall.ForkLock.Unlock()

	if err1 == syscall.EINVAL {
		panic("OS returned EINVAL. Make sure your kernel configuration includes all CONFIG_*_NS options.")
	} else if err1 != 0 {
		panic(err1)
	}

	// parent will get the pid, child will be 0
	if int(r1) != 0 {
		// Parent process here
		proc, procerr := os.FindProcess(int(r1))
		if procerr != nil {
			fmt.Fprintf(os.Stderr, "Failed waiting for child: %s\n", strconv.Itoa(int(r1)))
			panic(procerr)
		}
		// FIXME Race condition
		err = addProcessToCgroupsForContainer(containerSha, proc.Pid)

		doChrootChwd(rootfd, cwdfd)

		_, _ = proc.Wait()
		// FIXME: Deal with SIGSTOP on the child in the same way nsenter does?
		/* FIXME: Wait can detect if the child (immediately) fails, but better to do
		that reporting in the child process? Not sure, don't like throwing away err */
		/*if !pstate.Exited() {
			panic("Child has NOT exited")
		}*/
		return nil
	}

	// We're definitely in the child process by the time we get here.
	doChrootChwd(rootfd, cwdfd)

	// Drop capabilities except those in the whitelist, from https://github.com/docker/docker/blob/master/daemon/execdriver/native/template/default_template.go
	cape := capabilities.DropBoundingSet([]string{
		"CHOWN",
		"DAC_OVERRIDE",
		"FSETID",
		"FOWNER",
		//"MKNOD",
		//"NET_RAW",
		//"SETGID",
		//"SETUID",
		"SETFCAP",
		"SETPCAP",
		"NET_BIND_SERVICE",
		"SYS_CHROOT",
		"KILL",
		"AUDIT_WRITE",
	})
	if cape != nil {
		panic(cape)
	}

	// Drop groups, set to the primary group of the user.
	// TODO: Add user's other groups from /etc/group?
	if gid > 0 {
		err = syscall.Setgroups([]int{}) // drop supplementary groups
		if err != nil {
			panic("setgroups failed")
		}
		err = syscall.Setgid(gid)
		if err != nil {
			panic("setgid failed")
		}
	}
	// Change uid from root down to the actual user
	if uid > 0 {
		err = syscall.Setuid(uid)
		if err != nil {
			panic("setuid failed")
		}
	}

	// Exec their real shell
	// TODO: Add the ability to have arguments for the shell from config
	// TODO: Add the ability to trim environment and/or add to environment (kinda) like sudo does
	args := []string{shell}
	env := os.Environ()
	execErr := syscall.Exec(shell, args, env)
	if execErr != nil {
		panic(execErr)
	}
	return execErr
}
