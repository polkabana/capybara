package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"./homebrew"
	"github.com/op/go-logging"
	"github.com/polkabana/go-dmr"
	"gopkg.in/gcfg.v1"
)

type DmrId struct {
	ID       uint32
	Callsign string
	Alias    string
}

var (
	hb      *homebrew.Homebrew
	DmrIds  []*DmrId
	mutHttp = &sync.Mutex{}
	log     = logging.MustGetLogger("capybara")
)

func reloadDmrIdInfo() {
	for {
		log.Debug("reload DMR IDs")
		loadDmrIdInfo()
		time.Sleep(time.Hour)
	}
}

func loadDmrIdInfo() {
	var newDmrIds []*DmrId
	content, err := ioutil.ReadFile(homebrew.Config.General.DMRIDs)
	if err == nil {
		ids := strings.Split(string(content), "\n")
		for _, line := range ids {
			record := strings.Split(line, "\t")

			id, err := strconv.Atoi(strings.TrimSpace(record[0]))
			if err == nil {
				dmrid := &DmrId{
					ID:       uint32(id),
					Callsign: record[1],
					Alias:    record[2],
				}

				newDmrIds = append(newDmrIds, dmrid)
			}
		}

		DmrIds = newDmrIds
		log.Debug("DMR IDs loaded")
	}
}

func getDmrIdInfo(id uint32) (string, string) {
	for _, dmrid := range DmrIds {
		if dmrid.ID == id {
			return dmrid.Callsign, dmrid.Alias
		}
	}
	return "", ""
}

func httpIndex(w http.ResponseWriter, r *http.Request) {
	mutHttp.Lock()
	defer mutHttp.Unlock()

	content, err := ioutil.ReadFile("index.html")
	if err == nil {
		fmt.Fprintf(w, string(content))
	}

}

func httpPeers(w http.ResponseWriter, r *http.Request) {
	mutHttp.Lock()
	defer mutHttp.Unlock()

	w.Header().Set("Content-Type", "application/json")

	var peers []string
	for _, peer := range hb.GetPeers() {
		if peer.Incoming && peer.Status == homebrew.AuthDone {
			data := struct {
				ID       uint32
				Callsign string
				Location string
			}{
				ID:       peer.ID,
				Callsign: "",
				Location: "",
			}

			if peer.Config != nil {
				data.Callsign = peer.Config.Callsign
				data.Location = peer.Config.Location
			}

			b, err := json.Marshal(data)
			if err == nil {
				peers = append(peers, string(b))
			}
		}
	}

	fmt.Fprintf(w, "[%s]", strings.Join(peers, ",\n"))
}

func httpLastHeard(w http.ResponseWriter, r *http.Request) {
	mutHttp.Lock()
	defer mutHttp.Unlock()

	w.Header().Set("Content-Type", "application/json")

	var calls []string
	for _, call := range hb.GetCalls() {

		callsign, alias := getDmrIdInfo(call.SrcID)

		t := time.Unix(int64(call.Time/1000), 0)
		timeString := t.Format("15:04:05 02-Jan-2006")

		data := struct {
			Src         uint32
			SrcCallsign string
			SrcAlias    string
			Dst         uint32
			TS          uint8
			Type        string
			Time        string
			Duration    uint32
		}{
			Src:         call.SrcID,
			SrcCallsign: callsign,
			SrcAlias:    alias,
			Dst:         call.DstID,
			TS:          call.Timeslot,
			Type:        dmr.CallTypeShortName[call.CallType],
			Time:        timeString,
			Duration:    call.Duration,
		}

		b, err := json.Marshal(data)
		if err == nil {
			calls = append(calls, string(b))
		}
	}

	fmt.Fprintf(w, "[%s]", strings.Join(calls, ",\n"))
}

func httpServer() {
	http.HandleFunc("/", httpIndex)
	http.HandleFunc("/peers.json", httpPeers)
	http.HandleFunc("/lh.json", httpLastHeard)

	for {
		if err := http.ListenAndServe(fmt.Sprintf("%s:%d", homebrew.Config.General.IP, homebrew.Config.General.HTTPPort), nil); err != nil {
			log.Errorf("HTTP failed: %s\n", err.Error())
		}
	}
}

func main() {
	err := gcfg.ReadFileInto(&homebrew.Config, "capybara.cfg")
	if err != nil {
		println("Can't open config file")
		println(err.Error())
		return
	}

	logFormatter := logging.MustStringFormatter("%{level:.1s} %{time:01-02 15:04:05} %{message}")

	fileLog, _ := os.OpenFile(homebrew.Config.General.LogName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	defer fileLog.Close()

	fileBackend := logging.NewLogBackend(fileLog, "", 0)
	fileBackendFormatter := logging.NewBackendFormatter(fileBackend, logFormatter)
	fileBackendLeveled := logging.AddModuleLevel(fileBackendFormatter)
	fileBackendLeveled.SetLevel(logging.ERROR, "")

	switch strings.ToUpper(homebrew.Config.General.LogLevel) {
	case "CRITICAL":
		fileBackendLeveled.SetLevel(logging.CRITICAL, "")
		break
	case "ERROR":
		fileBackendLeveled.SetLevel(logging.ERROR, "")
		break
	case "WARNING":
		fileBackendLeveled.SetLevel(logging.WARNING, "")
		break
	case "NOTICE":
		fileBackendLeveled.SetLevel(logging.NOTICE, "")
		break
	case "INFO":
		fileBackendLeveled.SetLevel(logging.INFO, "")
		break
	case "DEBUG":
		fileBackendLeveled.SetLevel(logging.DEBUG, "")
		break
	}

	stdBackend := logging.NewLogBackend(os.Stderr, "", 0)
	stdBackendFormatter := logging.NewBackendFormatter(stdBackend, logFormatter)
	stdBackendLeveled := logging.AddModuleLevel(stdBackendFormatter)
	stdBackendLeveled.SetLevel(logging.DEBUG, "")

	logging.SetBackend(fileBackendLeveled, stdBackendFormatter)

	log.Infof("Capybara start\n")

	addr := &net.UDPAddr{
		IP:   net.ParseIP(homebrew.Config.General.IP),
		Port: homebrew.Config.General.Port,
	}

	hb, err = homebrew.New(homebrew.Config.General.ID, addr)
	if err != nil {
		log.Error(err.Error())
		return
	}

	if homebrew.Config.General.EnableHTTP {
		go httpServer()
	}

	if _, err := os.Stat(homebrew.Config.General.DMRIDs); err == nil {
		go reloadDmrIdInfo()
	} else {
		log.Errorf("File %s does not exis\n", homebrew.Config.General.DMRIDs)
	}

	for {
		if err := hb.ListenAndServe(); err != nil {
			log.Error(err.Error())
		}
	}
}
