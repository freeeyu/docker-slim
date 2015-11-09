package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/mangos"
	"github.com/gdamore/mangos/protocol/pub"
	"github.com/gdamore/mangos/protocol/rep"
	//"github.com/gdamore/mangos/transport/ipc"
	"bitbucket.org/madmo/fanotify"
	"github.com/cloudimmunity/pdiscover"
	"github.com/gdamore/mangos/transport/tcp"
)

func failOnError(err error) {
	if err != nil {
		log.Fatalln("launcher: ERROR =>", err)
	}
}

func failWhen(cond bool, msg string) {
	if cond {
		log.Fatalln("launcher: ERROR =>", msg)
	}
}

func myFileDir() string {
	dirName, err := filepath.Abs(filepath.Dir(os.Args[0]))
	failOnError(err)
	return dirName
}

func fileDir(fileName string) string {
	dirName, err := filepath.Abs(filepath.Dir(fileName))
	failOnError(err)
	return dirName
}

///////////////////////////////////////////////////////////////////////////////

var doneChan chan struct{}

var cmdChannelAddr = "tcp://0.0.0.0:65501"

//var cmdChannelAddr = "ipc:///tmp/docker-slim-launcher.cmds.ipc"
//var cmdChannelAddr = "ipc:///opt/dockerslim/ipc/docker-slim-launcher.cmds.ipc"
var cmdChannel mangos.Socket

func newCmdServer(addr string) (mangos.Socket, error) {
	log.Println("alauncher: creating cmd server...")
	socket, err := rep.NewSocket()
	if err != nil {
		return nil, err
	}

	if err := socket.SetOption(mangos.OptionRecvDeadline, time.Second*3); err != nil {
		socket.Close()
		return nil, err
	}

	//socket.AddTransport(ipc.NewTransport())
	socket.AddTransport(tcp.NewTransport())
	if err := socket.Listen(addr); err != nil {
		socket.Close()
		return nil, err
	}

	return socket, nil
}

func runCmdServer(channel mangos.Socket, done <-chan struct{}) (<-chan string, error) {
	cmdChan := make(chan string)
	go func() {
		for {
			// Could also use sock.RecvMsg to get header
			log.Println("alauncher: cmd server - waiting for a command...")
			select {
			case <-done:
				log.Println("alauncher: cmd server - done...")
				return
			default:
				if rawCmd, err := channel.Recv(); err != nil {
					switch err {
					case mangos.ErrRecvTimeout:
						log.Println("alauncher: cmd server - timeout... ok")
					default:
						log.Println("alauncher: cmd server - error =>", err)
					}
				} else {
					cmd := string(rawCmd)
					log.Println("alauncher: cmd server - got a command =>", cmd)
					cmdChan <- cmd
					//for now just ack the command and process the command asynchronously
					//NOTE:
					//must reply before receiving the next message
					//otherwise nanomsg/mangos will be confused :-)
					monitorFinishReply := "ok"
					err = channel.Send([]byte(monitorFinishReply))
					if err != nil {
						log.Println("alauncher: cmd server - fail to send monitor.finish reply =>", err)
					}
				}
			}
		}
	}()

	return cmdChan, nil
}

func shutdownCmdChannel() {
	if cmdChannel != nil {
		cmdChannel.Close()
		cmdChannel = nil
	}
}

var evtChannelAddr = "tcp://0.0.0.0:65502"

//var evtChannelAddr = "ipc:///tmp/docker-slim-launcher.events.ipc"
//var evtChannelAddr = "ipc:///opt/dockerslim/ipc/docker-slim-launcher.events.ipc"
var evtChannel mangos.Socket

func newEvtPublisher(addr string) (mangos.Socket, error) {
	log.Println("alauncher: creating event publisher...")
	socket, err := pub.NewSocket()
	if err != nil {
		return nil, err
	}

	if err := socket.SetOption(mangos.OptionSendDeadline, time.Second*3); err != nil {
		socket.Close()
		return nil, err
	}

	//socket.AddTransport(ipc.NewTransport())
	socket.AddTransport(tcp.NewTransport())
	if err = socket.Listen(addr); err != nil {
		socket.Close()
		return nil, err
	}

	return socket, nil
}

func publishEvt(channel mangos.Socket, evt string) error {
	if err := channel.Send([]byte(evt)); err != nil {
		log.Printf("fail to publish '%v' event:%v\n", evt, err)
		return err
	}

	return nil
}

func shutdownEvtChannel() {
	if evtChannel != nil {
		evtChannel.Close()
		evtChannel = nil
	}
}

//////////////

func cleanupOnStartup() {
	if _, err := os.Stat("/tmp/docker-slim-launcher.cmds.ipc"); err == nil {
		if err := os.Remove("/tmp/docker-slim-launcher.cmds.ipc"); err != nil {
			fmt.Printf("Error removing unix socket %s: %s", "/tmp/docker-slim-launcher.cmds.ipc", err.Error())
		}
	}

	if _, err := os.Stat("/tmp/docker-slim-launcher.events.ipc"); err == nil {
		if err := os.Remove("/tmp/docker-slim-launcher.events.ipc"); err != nil {
			fmt.Printf("Error removing unix socket %s: %s", "/tmp/docker-slim-launcher.events.ipc", err.Error())
		}
	}
}

func cleanupOnShutdown() {
	//fmt.Println("cleanupOnShutdown()...")

	if doneChan != nil {
		close(doneChan)
		doneChan = nil
	}

	shutdownCmdChannel()
	shutdownEvtChannel()
}

//////////////

var signals = []os.Signal{
	os.Interrupt,
	syscall.SIGTERM,
	syscall.SIGQUIT,
	syscall.SIGHUP,
	syscall.SIGSTOP,
	syscall.SIGCONT,
}

func initSignalHandlers() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, signals...)
	go func() {
		sig := <-sigChan
		fmt.Printf(" launcher: cleanup on signal (%v)...\n", sig)
		cleanupOnShutdown()
		os.Exit(0)
	}()
}

///////////////////////////////////////////////////////////////////////////////

func sendPids(pidList []int) {
	pidsData, err := json.Marshal(pidList)
	failOnError(err)

	monitorSocket, err := net.Dial("unix", "/tmp/amonitor.sock")
	failOnError(err)
	defer monitorSocket.Close()

	monitorSocket.Write(pidsData)
	monitorSocket.Write([]byte("\n"))
}

/////////

type event struct {
	ID      uint32
	Pid     int32
	File    string
	IsRead  bool
	IsWrite bool
}

func check(err error) {
	if err != nil {
		log.Fatalln("monitor error:", err)
	}
}

type processInfo struct {
	Pid       int32  `json:"pid"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Cmd       string `json:"cmd"`
	Cwd       string `json:"cwd"`
	Root      string `json:"root"`
	ParentPid int32  `json:"ppid"`
}

type fileInfo struct {
	EventCount   uint32 `json:"event_count"`
	FirstEventID uint32 `json:"first_eid"`
	Name         string `json:"-"`
	ReadCount    uint32 `json:"reads,omitempty"`
	WriteCount   uint32 `json:"writes,omitempty"`
	ExeCount     uint32 `json:"execs,omitempty"`
}

type monitorReport struct {
	MonitorPid       int                             `json:"monitor_pid"`
	MonitorParentPid int                             `json:"monitor_ppid"`
	EventCount       uint32                          `json:"event_count"`
	MainProcess      *processInfo                    `json:"main_process"`
	Processes        map[string]*processInfo         `json:"processes"`
	ProcessFiles     map[string]map[string]*fileInfo `json:"process_files"`
}

func procFilePath(pid int, key string) string {
	return fmt.Sprintf("/proc/%v/%v", pid, key)
}

func getProcessInfo(pid int32) (*processInfo, error) {
	info := &processInfo{Pid: pid}
	var err error

	info.Path, err = os.Readlink(procFilePath(int(pid), "exe"))
	if err != nil {
		return nil, err
	}

	info.Cwd, err = os.Readlink(procFilePath(int(pid), "cwd"))
	if err != nil {
		return nil, err
	}

	info.Root, err = os.Readlink(procFilePath(int(pid), "root"))
	if err != nil {
		return nil, err
	}

	rawCmdline, err := ioutil.ReadFile(procFilePath(int(pid), "cmdline"))
	if err != nil {
		return nil, err
	}

	if len(rawCmdline) > 0 {
		rawCmdline = bytes.TrimRight(rawCmdline, "\x00")
		//NOTE: later/future (when we do more app analytics)
		//split rawCmdline and resolve the "entry point" (exe or cmd param)
		info.Cmd = string(bytes.Replace(rawCmdline, []byte("\x00"), []byte(" "), -1))
	}

	//note: will need to get "environ" at some point :)
	//rawEnviron, err := ioutil.ReadFile(procFilePath(int(pid), "environ"))
	//if err != nil {
	//	return nil, err
	//}
	//if len(rawEnviron) > 0 {
	//	rawEnviron = bytes.TrimRight(rawEnviron,"\x00")
	//	info.Env = strings.Split(string(rawEnviron),"\x00")
	//}

	stat, err := ioutil.ReadFile(procFilePath(int(pid), "stat"))
	var procPid int
	var procName string
	var procStatus string
	var procPpid int
	fmt.Sscanf(string(stat), "%d %s %s %d", &procPid, &procName, &procStatus, &procPpid)

	info.Name = procName[1 : len(procName)-1]
	info.ParentPid = int32(procPpid)

	return info, nil
}

//func listenEvents(mountPoint string, stopChan chan bool) chan map[event]bool {
func listenEvents(mountPoint string, stopChan chan bool) <-chan *monitorReport {
	log.Println("monitor: listenEvents start")

	nd, err := fanotify.Initialize(fanotify.FAN_CLASS_NOTIF, os.O_RDONLY)
	check(err)
	err = nd.Mark(fanotify.FAN_MARK_ADD|fanotify.FAN_MARK_MOUNT,
		fanotify.FAN_MODIFY|fanotify.FAN_ACCESS|fanotify.FAN_OPEN, -1, mountPoint)
	check(err)

	//eventsChan := make(chan map[event]bool, 1)
	eventsChan := make(chan *monitorReport, 1)

	go func() {
		log.Println("monitor: listenEvents worker starting")
		//events := make(map[event]bool, 1)
		report := &monitorReport{
			MonitorPid:       os.Getpid(),
			MonitorParentPid: os.Getppid(),
			ProcessFiles:     make(map[string]map[string]*fileInfo),
		}

		eventChan := make(chan event)
		go func() {
			var eventID uint32

			for {
				data, err := nd.GetEvent()
				check(err)
				//log.Printf("TMP: monitor: listenEvents: data.Mask =>%x\n",data.Mask)

				if (data.Mask & fanotify.FAN_Q_OVERFLOW) == fanotify.FAN_Q_OVERFLOW {
					log.Println("monitor: listenEvents: overflow event")
					continue
				}

				doNotify := false
				isRead := false
				isWrite := false

				if (data.Mask & fanotify.FAN_OPEN) == fanotify.FAN_OPEN {
					//log.Println("TMP: monitor: listenEvents: file open")
					doNotify = true
				}

				if (data.Mask & fanotify.FAN_ACCESS) == fanotify.FAN_ACCESS {
					//log.Println("TMP: monitor: listenEvents: file read")
					isRead = true
					doNotify = true
				}

				if (data.Mask & fanotify.FAN_MODIFY) == fanotify.FAN_MODIFY {
					//log.Println("TMP: monitor: listenEvents: file write")
					isWrite = true
					doNotify = true
				}

				path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", data.File.Fd()))
				check(err)
				//log.Println("TMP: monitor: listenEvents: file path =>",path)

				data.File.Close()
				if doNotify {
					eventID++
					e := event{ID: eventID, Pid: data.Pid, File: path, IsRead: isRead, IsWrite: isWrite}
					eventChan <- e
				}
			}
		}()

		s := false
		for !s {
			select {
			//case <-time.After(110 * time.Second):
			//	log.Println("monitor: listenEvents - event timeout...")
			//	s = true
			case <-stopChan:
				log.Println("monitor: listenEvents stopping...")
				s = true
			case e := <-eventChan:
				report.EventCount++
				//log.Println("TMP: monitor: listenEvents: event ",report.EventCount)

				if e.ID == 1 {
					//first event represents the main process
					if pinfo, err := getProcessInfo(e.Pid); (err == nil) && (pinfo != nil) {
						report.MainProcess = pinfo
						report.Processes = make(map[string]*processInfo)
						report.Processes[strconv.Itoa(int(e.Pid))] = pinfo
						//log.Println("TMP: monitor: listenEvents: (1) adding pi for ",
						//	e.Pid,"info:",report.Processes[strconv.Itoa(int(e.Pid))])
					}
				} else {
					if _, ok := report.Processes[strconv.Itoa(int(e.Pid))]; !ok {
						if pinfo, err := getProcessInfo(e.Pid); (err == nil) && (pinfo != nil) {
							report.Processes[strconv.Itoa(int(e.Pid))] = pinfo
							//log.Println("TMP: monitor: listenEvents: (2) adding pi for ",
							//	e.Pid,"info:",report.Processes[strconv.Itoa(int(e.Pid))])
						}
					}
				}

				if _, ok := report.ProcessFiles[strconv.Itoa(int(e.Pid))]; !ok {
					report.ProcessFiles[strconv.Itoa(int(e.Pid))] = make(map[string]*fileInfo)
					//log.Println("TMP: monitor: listenEvents: adding pf for ",e.Pid)
				}

				if existingFi, ok := report.ProcessFiles[strconv.Itoa(int(e.Pid))][e.File]; !ok {
					fi := &fileInfo{
						EventCount: 1,
						Name:       e.File,
					}

					if e.IsRead {
						fi.ReadCount = 1
					}

					if e.IsWrite {
						fi.WriteCount = 1
					}

					if pi, ok := report.Processes[strconv.Itoa(int(e.Pid))]; ok && (e.File == pi.Path) {
						fi.ExeCount = 1
					}

					report.ProcessFiles[strconv.Itoa(int(e.Pid))][e.File] = fi
				} else {
					existingFi.EventCount++

					if e.IsRead {
						existingFi.ReadCount++
					}

					if e.IsWrite {
						existingFi.WriteCount++
					}

					if pi, ok := report.Processes[strconv.Itoa(int(e.Pid))]; ok && (e.File == pi.Path) {
						existingFi.ExeCount++
					}
				}

				//log.Printf("monitor: listenEvents event => %#v\n", e)
			}
		}

		log.Printf("monitor: listenEvents - sending report (processed %v events)...\n", report.EventCount)
		eventsChan <- report
	}()

	return eventsChan
}

func monitorProcess(stop chan bool) chan map[int][]int {
	log.Println("monitor: monitorProcess start")

	watcher, err := pdiscover.NewAllWatcher(pdiscover.PROC_EVENT_ALL)
	check(err)

	forksChan := make(chan map[int][]int, 1)

	go func() {
		forks := make(map[int][]int)
		s := false
		for !s {
			select {
			case <-stop:
				s = true
			case ev := <-watcher.Fork:
				forks[ev.ParentPid] = append(forks[ev.ParentPid], ev.ChildPid)
			case <-watcher.Exec:
			case <-watcher.Exit:
			case err := <-watcher.Error:
				log.Println("error: ", err)
				panic(err)
			}
		}
		forksChan <- forks
		watcher.Close()
	}()

	return forksChan
}

func getFiles(events chan map[event]bool, pidsMap chan map[int][]int, pids chan []int) []string {
	p := <-pids
	pm := <-pidsMap
	e := <-events
	allPids := make(map[int]bool, 0)

	for _, v := range p {
		allPids[v] = true
		for _, pl := range pm[v] {
			allPids[pl] = true
		}
	}

	var files []string
	for k := range e {
		_, found := allPids[int(k.Pid)]
		if found {
			files = append(files, k.File)
		}
	}
	return files
}

func getFilesAll(events chan map[event]bool) []string {
	log.Println("launcher: getFilesAll - getting events...")
	e := <-events
	log.Println("launcher: getFilesAll - event count =>", len(e))

	var files []string
	for k := range e {
		log.Println("launcher: getFilesAll - adding file =>", k.File)
		files = append(files, k.File)
	}
	return files
}

func filesToInodes(files []string) []int {
	cmd := "/usr/bin/stat"
	args := []string{"-L", "-c", "%i"}
	args = append(args, files...)
	var inodes []int

	c := exec.Command(cmd, args...)
	out, _ := c.Output()
	c.Wait()
	for _, i := range strings.Split(string(out), "\n") {
		inode, err := strconv.Atoi(strings.TrimSpace(i))
		if err != nil {
			continue
		}
		inodes = append(inodes, inode)
	}
	return inodes
}

func findSymlinks(files []string, mp string) map[string]*artifactProps {
	cmd := "/usr/bin/find"
	args := []string{"-L", mp, "-mount", "-printf", "%i %p\n"}
	c := exec.Command(cmd, args...)
	out, _ := c.Output()
	c.Wait()

	inodes := filesToInodes(files)
	inodeToFiles := make(map[int][]string)

	for _, v := range strings.Split(string(out), "\n") {
		v = strings.TrimSpace(v)
		info := strings.Split(v, " ")
		inode, err := strconv.Atoi(info[0])
		if err != nil {
			continue
		}
		inodeToFiles[inode] = append(inodeToFiles[inode], info[1])
	}

	result := make(map[string]*artifactProps, 0)
	for _, i := range inodes {
		v := inodeToFiles[i]
		for _, f := range v {
			result[f] = nil
		}
	}
	return result
}

func cpFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		log.Println("launcher: monitor - cp - error opening source file =>", src)
		return err
	}
	defer s.Close()

	dstDir := fileDir(dst)
	err = os.MkdirAll(dstDir, 0777)
	if err != nil {
		log.Println("launcher: monitor - dir error =>", err)
	}

	d, err := os.Create(dst)
	if err != nil {
		log.Println("launcher: monitor - cp - error opening dst file =>", dst)
		return err
	}

	srcFileInfo, err := s.Stat()
	if err == nil {
		//if (srcFileInfo.Mode() & 0111) > 0 {
		//	log.Println("TMP: launcher: monitor - cp: executable =>",src,"|perms =>",srcFileInfo.Mode())
		//}

		d.Chmod(srcFileInfo.Mode())
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func getFileHash(artifactFileName string) (string, error) {
	fileData, err := ioutil.ReadFile(artifactFileName)
	if err != nil {
		return "", err
	}

	hash := sha1.Sum(fileData)
	return hex.EncodeToString(hash[:]), nil
}

func getDataType(artifactFileName string) (string, error) {
	//TODO: use libmagic (pure impl)
	var cerr bytes.Buffer
	var cout bytes.Buffer

	cmd := exec.Command("file", artifactFileName)
	cmd.Stderr = &cerr
	cmd.Stdout = &cout

	if err := cmd.Start(); err != nil {
		return "", err
	}

	if err := cmd.Wait(); err != nil {
		err = fmt.Errorf("Error getting data type: %s / stderr: %s", err, cerr.String())
		return "", err
	}

	if typeInfo := strings.Split(strings.TrimSpace(cout.String()), ":"); len(typeInfo) > 1 {
		return strings.TrimSpace(typeInfo[1]), nil
	}

	return "unknown", nil
}

type artifactType int

const (
	dirArtifactType     = 1
	fileArtifactType    = 2
	symlinkArtifactType = 3
	unknownArtifactType = 99
)

var artifactTypeNames = map[artifactType]string{
	dirArtifactType:     "Dir",
	fileArtifactType:    "File",
	symlinkArtifactType: "Symlink",
	unknownArtifactType: "Unknown",
}

func (t artifactType) String() string {
	return artifactTypeNames[t]
}

type artifactProps struct {
	FileType artifactType    `json:"-"`
	FilePath string          `json:"file_path"`
	Mode     os.FileMode     `json:"-"`
	ModeText string          `json:"mode"`
	LinkRef  string          `json:"link_ref,omitempty"`
	Flags    map[string]bool `json:"flags,omitempty"`
	DataType string          `json:"data_type,omitempty"`
	FileSize int64           `json:"file_size"`
	Sha1Hash string          `json:"sha1_hash,omitempty"`
	AppType  string          `json:"app_type,omitempty"`
}

func (p *artifactProps) MarshalJSON() ([]byte, error) {
	type artifactPropsType artifactProps
	return json.Marshal(&struct {
		FileTypeStr string `json:"file_type"`
		*artifactPropsType
	}{
		FileTypeStr:       p.FileType.String(),
		artifactPropsType: (*artifactPropsType)(p),
	})
}

type artifactStore struct {
	storeLocation string
	monReport     *monitorReport
	rawNames      map[string]*artifactProps
	nameList      []string
	resolve       map[string]struct{}
	linkMap       map[string]*artifactProps
	fileMap       map[string]*artifactProps
}

func newArtifactStore(storeLocation string,
	monReport *monitorReport,
	rawNames map[string]*artifactProps) *artifactStore {
	store := &artifactStore{
		storeLocation: storeLocation,
		monReport:     monReport,
		rawNames:      rawNames,
		nameList:      make([]string, 0, len(rawNames)),
		resolve:       map[string]struct{}{},
		linkMap:       map[string]*artifactProps{},
		fileMap:       map[string]*artifactProps{},
	}

	return store
}

func (p *artifactStore) getArtifactFlags(artifactFileName string) map[string]bool {
	flags := map[string]bool{}
	for _, processFileMap := range p.monReport.ProcessFiles {
		if finfo, ok := processFileMap[artifactFileName]; ok {
			if finfo.ReadCount > 0 {
				flags["R"] = true
			}

			if finfo.WriteCount > 0 {
				flags["W"] = true
			}

			if finfo.ExeCount > 0 {
				flags["X"] = true
			}
		}
	}

	if len(flags) < 1 {
		return nil
	}

	return flags
}

func (p *artifactStore) prepareArtifact(artifactFileName string) {
	srcLinkFileInfo, err := os.Lstat(artifactFileName)
	if err != nil {
		log.Printf("prepareArtifact - artifact don't exist: %v (%v)\n", artifactFileName, os.IsNotExist(err))
		return
	}

	p.nameList = append(p.nameList, artifactFileName)

	props := &artifactProps{
		FilePath: artifactFileName,
		Mode:     srcLinkFileInfo.Mode(),
		ModeText: srcLinkFileInfo.Mode().String(),
		FileSize: srcLinkFileInfo.Size(),
	}

	props.Flags = p.getArtifactFlags(artifactFileName)

	switch {
	case srcLinkFileInfo.Mode().IsRegular():
		//log.Printf("prepareArtifact - is a regular file")
		props.FileType = fileArtifactType
		props.Sha1Hash, _ = getFileHash(artifactFileName)
		props.DataType, _ = getDataType(artifactFileName)
		p.fileMap[artifactFileName] = props
		p.rawNames[artifactFileName] = props
	case (srcLinkFileInfo.Mode() & os.ModeSymlink) != 0:
		//log.Printf("prepareArtifact - is a symlink")
		linkRef, err := os.Readlink(artifactFileName)
		if err != nil {
			log.Printf("prepareArtifact - error getting reference for symlink: %v\n", artifactFileName)
			return
		}

		//log.Printf("prepareArtifact(%s): src is a link! references => %s\n", artifactFileName, linkRef)
		props.FileType = symlinkArtifactType
		props.LinkRef = linkRef

		if _, ok := p.rawNames[linkRef]; !ok {
			p.resolve[linkRef] = struct{}{}
		}

		p.linkMap[artifactFileName] = props
		p.rawNames[artifactFileName] = props

	case srcLinkFileInfo.Mode().IsDir():
		log.Printf("prepareArtifact - is a directory (shouldn't see it)")
		props.FileType = dirArtifactType
	default:
		log.Printf("prepareArtifact - other type (shouldn't see it)")
	}
}

func (p *artifactStore) prepareArtifacts() {
	for artifactFileName := range p.rawNames {
		//log.Printf("prepareArtifacts - artifact => %v\n",artifactFileName)
		p.prepareArtifact(artifactFileName)
	}

	p.resolveLinks()
}

func (p *artifactStore) resolveLinks() {
	for name := range p.resolve {
		_ = name
		//log.Println("resolveLinks - resolving:", name)
		//TODO
	}
}

func (p *artifactStore) saveArtifacts() {
	for fileName := range p.fileMap {
		filePath := fmt.Sprintf("%s/files%s", p.storeLocation, fileName)
		//log.Println("saveArtifacts - saving file data =>", filePath)
		err := cpFile(fileName, filePath)
		if err != nil {
			log.Println("saveArtifacts - error saving file =>", err)
		}
	}

	for linkName, linkProps := range p.linkMap {
		linkPath := fmt.Sprintf("%s/files%s", p.storeLocation, linkName)
		linkDir := fileDir(linkPath)
		err := os.MkdirAll(linkDir, 0777)
		if err != nil {
			log.Println("saveArtifacts - dir error =>", err)
			continue
		}
		err = os.Symlink(linkProps.LinkRef, linkPath)
		if err != nil {
			log.Println("saveArtifacts - symlink create error ==>", err)
		}
	}
}

type imageReport struct {
	Files []*artifactProps `json:"files"`
}

type reportInfo struct {
	Monitor *monitorReport `json:"monitor"`
	Image   imageReport    `json:"image"`
}

func (p *artifactStore) saveReport() {
	sort.Strings(p.nameList)

	report := reportInfo{Monitor: p.monReport}

	for _, fname := range p.nameList {
		report.Image.Files = append(report.Image.Files, p.rawNames[fname])
	}

	artifactDirName := "/opt/dockerslim/artifacts"
	reportName := "creport.json"

	_, err := os.Stat(artifactDirName)
	if os.IsNotExist(err) {
		os.MkdirAll(artifactDirName, 0777)
		_, err = os.Stat(artifactDirName)
		check(err)
	}

	reportFilePath := filepath.Join(artifactDirName, reportName)
	log.Println("launcher: monitor - saving report to", reportFilePath)

	reportData, err := json.MarshalIndent(report, "", "  ")
	check(err)

	err = ioutil.WriteFile(reportFilePath, reportData, 0644)
	check(err)
}

func saveResults(monReport *monitorReport, fileNames map[string]*artifactProps) {
	artifactDirName := "/opt/dockerslim/artifacts"

	artifactStore := newArtifactStore(artifactDirName, monReport, fileNames)
	artifactStore.prepareArtifacts()
	artifactStore.saveArtifacts()
	artifactStore.saveReport()
}

func writeData(monitorFileName string, files map[string]bool) {
	artifactDirName := "/opt/dockerslim/artifacts"
	/*
		err = os.MkdirAll(artifactDir, 0777)
		if err != nil {
			log.Println("launcher: monitor - artifact dir error =>", err)
		}
	*/
	_, err := os.Stat(artifactDirName)
	if os.IsNotExist(err) {
		os.MkdirAll(artifactDirName, 0777)
		_, err = os.Stat(artifactDirName)
		check(err)
	}

	resultFile := filepath.Join(artifactDirName, monitorFileName)

	log.Println("launcher: monitor - saving results to", resultFile)
	f, err := os.Create(resultFile)
	check(err)
	defer f.Close()
	w := bufio.NewWriter(f)

	for k := range files {
		w.WriteString(k)
		w.WriteString("\n")
	}
	w.Flush()
}

func monitor(stopWork chan bool, stopWorkAck chan bool, pids chan []int) {
	log.Println("launcher: monitor starting...")
	mountPoint := "/"
	//file := "/opt/dockerslim/artifacts/monitor_results"
	//monitorFileName := "monitor_results"

	//stopEvents := make(chan bool, 1)
	stopEvents := make(chan bool)
	//events := listenEvents(mountPoint, stopEvents)
	reportChan := listenEvents(mountPoint, stopEvents)

	//stop_process := make(chan bool, 1)
	//pidsMap := monitorProcess(stop_process)

	go func() {
		log.Println("launcher: monitor - waiting to stop monitoring...")
		<-stopWork
		log.Println("launcher: monitor - stop message...")
		stopEvents <- true
		//stop_process <- true
		log.Println("launcher: monitor - processing data...")
		//files := getFiles(events, pidsMap, pids)
		//NOTE/TODO:
		//should use getFiles() though it won't work properly for apps that spawn processes
		//because the pid list only contains the pid for the main app process
		//(when process monitoring is not used)
		//files := getFilesAll(events)
		report := <-reportChan

		//processCount := len(report.ProcessFiles)
		fileCount := 0
		for _, processFileMap := range report.ProcessFiles {
			fileCount += len(processFileMap)
		}
		fileList := make([]string, 0, fileCount)
		for _, processFileMap := range report.ProcessFiles {
			for fpath := range processFileMap {
				fileList = append(fileList, fpath)
			}
		}

		allFilesMap := findSymlinks(fileList, mountPoint)
		//writeData(monitorFileName, allFilesList)
		saveResults(report, allFilesMap)
		stopWorkAck <- true
	}()
}

/////////

func main() {
	log.Printf("launcher: args => %#v\n", os.Args)
	failWhen(len(os.Args) < 2, "missing app information")

	dirName, err := os.Getwd()
	failOnError(err)
	log.Printf("launcher: cwd => %#v\n", dirName)

	appName := os.Args[1]
	var appArgs []string
	if len(os.Args) > 2 {
		appArgs = os.Args[2:]
	}

	initSignalHandlers()
	defer func() {
		fmt.Println("defered cleanup on shutdown...")
		cleanupOnShutdown()
	}()

	/*
	   monitorPath := fmt.Sprintf("%s/amonitor",myFileDir())
	   log.Printf("launcher: start monitor (%v)\n",monitorPath)
	   monitorArgs := []string{
	       "-file",
	       "/opt/dockerslim/monitor_results",
	       "-socket",
	       "/tmp/amonitor.sock",
	       "-mount",
	       "/",
	   }
	   monitor := exec.Command(monitorPath,monitorArgs...)
	   err = monitor.Start()
	   failOnError(err)
	   defer monitor.Wait()
	*/

	monDoneChan := make(chan bool, 1)
	monDoneAckChan := make(chan bool)
	pidsChan := make(chan []int, 1)
	monitor(monDoneChan, monDoneAckChan, pidsChan)

	log.Printf("launcher: start target app => %v %#v\n", appName, appArgs)

	app := exec.Command(appName, appArgs...)
	app.Dir = dirName
	app.Stdout = os.Stdout
	app.Stderr = os.Stderr

	err = app.Start()
	failOnError(err)
	defer app.Wait()
	log.Printf("launcher: target app pid => %v\n", app.Process.Pid)
	time.Sleep(3 * time.Second)

	//sendPids([]int{app.Process.Pid})
	pidsChan <- []int{app.Process.Pid}

	log.Println("alauncher: waiting for monitor:")
	/*
			//TODO: fix the hard coded timeout
			endTime := time.After(130 * time.Second)
			work := 0

		doneRunning:
			for {
				select {
				case <-endTime:
					log.Println("\nalauncher: done waiting :)")
					break doneRunning
				case <-time.After(time.Second * 5):
					work++
					log.Printf(".")
				}
			}
	*/

	doneChan = make(chan struct{})
	evtChannel, err = newEvtPublisher(evtChannelAddr)
	failOnError(err)
	cmdChannel, err = newCmdServer(cmdChannelAddr)
	failOnError(err)

	cmdChan, err := runCmdServer(cmdChannel, doneChan)
	failOnError(err)
doneRunning:
	for {
		select {
		case cmd := <-cmdChan:
			log.Println("\nalauncher: command =>", cmd)
			switch cmd {
			case "monitor.finish":
				log.Println("alauncher: stopping monitor...")
				break doneRunning
			default:
				log.Println("alauncher: ignoring command =>", cmd)
			}

		case <-time.After(time.Second * 5):
			log.Printf(".")
		}
	}

	log.Println("launcher: stopping monitor...")
	//monitor.Process.Signal(syscall.SIGTERM)
	monDoneChan <- true
	log.Println("launcher: waiting for monitor to finish...")
	<-monDoneAckChan
	//time.Sleep(3 * time.Second)

	for ptry := 0; ptry < 3; ptry++ {
		log.Printf("launcher: trying to publish 'monitor.finish.completed' event (attempt %v)\n", ptry+1)
		err = publishEvt(evtChannel, "monitor.finish.completed")
		if err == nil {
			log.Println("launcher: published 'monitor.finish.completed'")
			break
		}

		switch err {
		case mangos.ErrRecvTimeout:
			log.Println("launcher: publish event timeout... ok")
		default:
			log.Println("launcher: publish event error =>", err)
		}
	}

	log.Println("launcher: done!")
}