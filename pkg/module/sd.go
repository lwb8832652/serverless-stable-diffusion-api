package module

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/config"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/datastore"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/log"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/utils"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SD_CONFIG         = "config.json"
	SD_START_TIMEOUT  = 5 * 60 * 1000 // 5min
	SD_DETECT_TIMEOUT = 500           // 500ms
	SD_REQUEST_WAIT   = 30 * 1000     // 30s
)

var (
	SDManageObj *SDManager
	once        sync.Once
)

type SDManager struct {
	pid             int
	port            string
	modelLoadedFlag bool
	restartFlag     bool
	stdout          io.ReadCloser
	endChan         chan struct{}
	signalIn        chan struct{}
	signalOut       chan struct{}
}

func NewSDManager(port string) *SDManager {
	SDManageObj = new(SDManager)
	SDManageObj.port = port
	SDManageObj.endChan = make(chan struct{}, 1)
	SDManageObj.signalIn = make(chan struct{}, 1)
	SDManageObj.signalOut = make(chan struct{})
	if err := SDManageObj.init(); err != nil {
		logrus.Error(err.Error())
	}
	return SDManageObj
}

func (s *SDManager) getEnv() []string {
	env := make([]string, 0)
	fileMgrToken := ""
	fileMgrEndpoint := ""
	if fileMgr := FuncManagerGlobal.GetFileMgr(); fileMgr != nil && fileMgr.EnvironmentVariables != nil {
		adminEnv := fileMgr.EnvironmentVariables
		if token := adminEnv["TOKEN"]; token != nil {
			fileMgrToken = *token
		}
		fileMgrEndpoint = GetHttpTrigger(*fileMgr.FunctionName)
	}
	env = append(
		os.Environ(),
		fmt.Sprintf("SERVERLESS_SD_FILEMGR_TOKEN=%s", fileMgrToken),
		fmt.Sprintf("SERVERLESS_SD_FILEMGR_DOMAIN=%s", fileMgrEndpoint))

	// not set DISABLE_HF_CHECK, default proxy enable
	if !config.ConfigGlobal.GetDisableHealthCheck() {
		env = append(env,
			"HTTP_PROXY=http://127.0.0.1:1080",
			"HTTPS_PROXY=http://127.0.0.1:1080",
		)
	}
	return env
}

func (s *SDManager) init() error {
	s.modelLoadedFlag = false
	sdStartTs := utils.TimestampMS()
	defer func() {
		sdEndTs := utils.TimestampMS()
		log.SDLogInstance.TraceFlow <- []string{config.TrackerKeyStableDiffusionStartup,
			fmt.Sprintf("sd start cost=%d", sdEndTs-sdStartTs)}
	}()
	// start sd
	// todo: 修改成windows启动方式
	execItem, err := utils.DoExecAsync(config.ConfigGlobal.SdShell, config.ConfigGlobal.SdPath, s.getEnv())
	if err != nil {
		return err
	}
	// init read sd log
	go func() {
		stdout := bufio.NewScanner(execItem.Stdout)
		defer execItem.Stdout.Close()
		for stdout.Scan() {
			select {
			case <-s.endChan:
				return
			default:
				logStr := stdout.Text()
				if !s.modelLoadedFlag && strings.HasPrefix(logStr, "Model loaded in") {
					s.modelLoadedFlag = true
				}
				log.SDLogInstance.LogFlow <- logStr
			}
		}
	}()
	s.pid = execItem.Pid
	s.stdout = execItem.Stdout
	// make sure sd started(port exist)
	if !utils.PortCheck(s.port, SD_START_TIMEOUT) {
		return errors.New("sd not start after 5min")
	}
	if os.Getenv(config.CHECK_MODEL_LOAD) != "" && strings.Contains(os.Getenv(config.SD_START_PARAMS), "--api") {
		// if api mode need blocking model loaded
		s.waitModelLoaded(SD_START_TIMEOUT)
	}
	once.Do(func() {
		go s.detectSdAlive()
	})
	return nil
}

// idle charge mode need check model
func (s *SDManager) waitModelLoaded(timeout int) {
	timeoutChan := time.After(time.Duration(timeout) * time.Millisecond)
	for {
		select {
		case <-timeoutChan:
			return
		default:
			if s.modelLoadedFlag && s.predictProbe() {
				return
			}
		}
	}
}

// predict one task, return true always
func (s *SDManager) predictProbe() bool {
	payload := map[string]interface{}{
		"prompt": "",
		"steps":  1,
		"height": 8,
		"width":  8,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(config.HTTP_POST,
		fmt.Sprintf("%s%s", config.ConfigGlobal.SdUrlPrefix,
			config.TXT2IMG), bytes.NewBuffer(body))
	client := &http.Client{}
	client.Do(req)
	return true
}

func (s *SDManager) detectSdAlive() {
	// SD_DETECT_TIMEOUT ms
	for {
		//s.KillAgentWithoutSd()
		s.WaitPortWork()
		time.Sleep(time.Duration(SD_DETECT_TIMEOUT) * time.Millisecond)
	}
}

func (s *SDManager) KillAgentWithoutSd() {
	if !checkSdExist(strconv.Itoa(s.pid)) && !utils.PortCheck(s.port, SD_DETECT_TIMEOUT) {
		//syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}
}

func (s *SDManager) WaitPortWork() {
	// sd not exist, kill
	if !checkSdExist(strconv.Itoa(s.pid)) && !utils.PortCheck(s.port, SD_DETECT_TIMEOUT) {
		logrus.Info("restart process....")
		s.init()
	}
}

// WaitSDRestartFinish blocking until sd restart finish
func (s *SDManager) WaitSDRestartFinish() {
	//select {
	//case <-s.signalIn:
	//	logrus.Info("blocking until restart finish")
	//	s.signalOut <- struct{}{}
	//default:
	//}
	if utils.PortCheck(s.port, SD_REQUEST_WAIT) {
		time.Sleep(time.Duration(1) * time.Second)
	}
}

func (s *SDManager) Close() {
	//syscall.Kill(-s.pid, syscall.SIGKILL)
	s.endChan <- struct{}{}
}

// UpdateSdConfig modify sd config.json sd_model_checkpoint and sd_vae
func UpdateSdConfig(configStore datastore.Datastore) error {
	// sdModel/sdVae from env
	sdModel := os.Getenv(config.MODEL_SD)
	if sdModel == "" {
		return errors.New("sd model not set in env")
	}
	var data []byte
	configPath := fmt.Sprintf("%s/%s", config.ConfigGlobal.SdPath, SD_CONFIG)
	// get sd config from remote
	configData, err := configStore.Get(ConfigDefaultKey, []string{datastore.KConfigVal})
	if err == nil && configData != nil && len(configData) > 0 {
		data = []byte(configData[datastore.KConfigVal].(string))
	} else {
		// get sd config from local
		fd, err := os.Open(configPath)
		if err != nil {
			return err
		}
		data, _ = ioutil.ReadAll(fd)
		fd.Close()
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	m["sd_model_checkpoint"] = sdModel
	m["sd_vae"] = "None"
	m["sd_checkpoint_hash"] = ""
	output, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		return err
	}
	// delete first
	utils.DeleteLocalFile(configPath)
	fdOut, err := os.OpenFile(configPath, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0775)
	defer fdOut.Close()

	fdOut.WriteString(string(output))
	return nil
}

func checkSdExist(pid string) bool {
	execItem := utils.DoExec("ps -ef|grep webui|grep -v agent|grep -v grep|awk '{print $2}'", "", nil)
	if strings.Trim(execItem.Output, "\n") == pid {
		return true
	}
	return false
}
