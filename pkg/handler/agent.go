package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/config"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/datastore"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/models"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/module"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/utils"
	"github.com/gin-gonic/gin"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type AgentHandler struct {
	taskStore   datastore.Datastore
	modelStore  datastore.Datastore
	configStore datastore.Datastore
	httpClient  *http.Client // the http client
	listenTask  *module.ListenDbTask
}

func NewAgentHandler(taskStore datastore.Datastore,
	modelStore datastore.Datastore, configStore datastore.Datastore,
	listenTask *module.ListenDbTask) *AgentHandler {
	return &AgentHandler{
		taskStore:   taskStore,
		modelStore:  modelStore,
		httpClient:  &http.Client{},
		listenTask:  listenTask,
		configStore: configStore,
	}
}

// Img2Img img to img predict
// (POST /img2img)
func (a *AgentHandler) Img2Img(c *gin.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	username := c.GetHeader(userKey)
	taskId := c.GetHeader(taskKey)
	configVer := c.GetHeader(versionKey)
	if username == "" || taskId == "" || configVer == "" {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}

	request := new(models.Img2ImgJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// update request OverrideSettings
	if err := a.updateRequest(request.OverrideSettings, username, configVer); err != nil {
		handleError(c, http.StatusInternalServerError, "please check config")
		return
	}
	// update task status
	a.taskStore.Update(taskId, map[string]interface{}{
		datastore.KTaskStatus: config.TASK_INPROGRESS,
	})
	// add cancel event task
	a.listenTask.AddTask(taskId, module.CancelListen, module.CancelEvent)
	// async progress
	go func() {
		if err := a.taskProgress(ctx, taskId); err != nil {
			log.Printf("update task progress error %s", err.Error())
		}
	}()

	body, err := json.Marshal(request)
	if err != nil {
		log.Println("request to json err=", err.Error())
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// predict task
	a.predictTask(username, taskId, config.IMG2IMG, body)
	log.Printf("task=%s predict success, request body=%s", taskId, string(body))
	c.JSON(http.StatusOK, gin.H{
		"message": "predict task success",
	})
}

// Txt2Img txt to img predict
// (POST /txt2img)
func (a *AgentHandler) Txt2Img(c *gin.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	username := c.GetHeader(userKey)
	taskId := c.GetHeader(taskKey)
	configVer := c.GetHeader(versionKey)
	if username == "" || taskId == "" || configVer == "" {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}

	request := new(models.Txt2ImgJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// update request OverrideSettings
	if err := a.updateRequest(request.OverrideSettings, username, configVer); err != nil {
		handleError(c, http.StatusInternalServerError, "please check config")
		return
	}
	// update task status
	a.taskStore.Update(taskId, map[string]interface{}{
		datastore.KTaskStatus: config.TASK_INPROGRESS,
	})
	// add cancel event task
	a.listenTask.AddTask(taskId, module.CancelListen, module.CancelEvent)
	// async progress
	go func() {
		if err := a.taskProgress(ctx, taskId); err != nil {
			log.Printf("update task progress error %s", err.Error())
		}
	}()

	body, err := json.Marshal(request)
	if err != nil {
		log.Println("request to json err=", err.Error())
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// predict task
	a.predictTask(username, taskId, config.TXT2IMG, body)
	log.Printf("task=%s predict success, request body=%s", taskId, string(body))
	c.JSON(http.StatusOK, gin.H{
		"message": "predict task success",
	})
}

func (a *AgentHandler) predictTask(user, taskId, path string, body []byte) error {
	url := fmt.Sprintf("%s%s", config.ConfigGlobal.SdUrlPrefix, path)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}

	body, err = io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return err
	}

	var result *models.Txt2ImgResult
	if err := json.Unmarshal(body, &result); err != nil {
		log.Println(err.Error())
		return err
	}
	params, err := json.Marshal(result.Parameters)
	if err != nil {
		log.Println("json:", err.Error())
	}
	var images []string
	if resp.StatusCode == requestOk {
		count := len(result.Images)
		for i := 1; i <= count; i++ {
			// test image save local
			//localPngName := fmt.Sprintf("%s%s_%d.png", config.ConfigGlobal.ImageOutputDir, taskId, i)
			//if err := outputImage(&localPngName, &result.Images[i-1]); err != nil {
			//	return fmt.Errorf("output image err=%s", err.Error())
			//}
			// upload image to oss
			ossPath := fmt.Sprintf("images/%s/%s_%d.png", user, taskId, i)
			if err := uploadImages(&ossPath, &result.Images[i-1]); err != nil {
				return fmt.Errorf("output image err=%s", err.Error())
			}

			images = append(images, ossPath)
		}
	}
	if err := a.taskStore.Update(taskId, map[string]interface{}{
		datastore.KTaskCode:        int64(resp.StatusCode),
		datastore.KTaskStatus:      config.TASK_FINISH,
		datastore.KTaskImage:       strings.Join(images, ","),
		datastore.KTaskParams:      string(params),
		datastore.KTaskInfo:        result.Info,
		datastore.KModelModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}); err != nil {
		log.Println(err.Error())
		return err
	}
	return nil
}

func (a *AgentHandler) taskProgress(ctx context.Context, taskId string) error {
	var isStart bool
	notifyDone := false
	for {
		select {
		case <-ctx.Done():
			notifyDone = true
		default:
			// Do nothing, go to the next
		}
		progressUrl := fmt.Sprintf("%s%s", config.ConfigGlobal.SdUrlPrefix, config.PROGRESS)
		req, _ := http.NewRequest("GET", progressUrl, nil)
		resp, err := a.httpClient.Do(req)
		if err != nil {
			return err
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()

		var result models.ProgressResult
		if err := json.Unmarshal(body, &result); err != nil {
			return err
		}

		// Get progress judge task done
		if isStart && result.Progress <= 0 {
			log.Printf("taskid=%s is done", taskId)
			return nil
		}
		if result.Progress > 0 {
			log.Println("progress:", result.Progress)
			// output to local
			if result.CurrentImage != "" {
				pngName := fmt.Sprintf("%s%s_progress.png", config.ConfigGlobal.ImageOutputDir, taskId)
				if err := outputImage(&pngName, &result.CurrentImage); err != nil {
					return fmt.Errorf("output image err=%s", err.Error())
				}
				result.CurrentImage = pngName
			}
			// write to db, struct to json str
			if resultStr, err := json.Marshal(result); err == nil {
				// modify key: current_image=>currentImage, eta_relative=>etaRelative
				resultByte := strings.Replace(string(resultStr), "current_image", "currentImage", 1)
				resultByte = strings.Replace(string(resultByte), "eta_relative", "etaRelative", 1)
				// Update the task progress to DB.
				if err = a.taskStore.Update(taskId, map[string]interface{}{
					datastore.KTaskProgressColumnName: string(resultByte),
					datastore.KTaskModifyTime:         fmt.Sprintf("%d", utils.TimestampS()),
				}); err != nil {
					log.Println("err:", err.Error())
				}
			}
			isStart = true
		}

		// notifyDone means the update-progress is notified by other goroutine to finish,
		// either because the task has been aborted or succeed.
		if notifyDone {
			log.Printf("the task %s is done, either success or failed", taskId)
			return nil
		}

		time.Sleep(config.PROGRESS_INTERVAL * time.Millisecond)
	}
}

func (a *AgentHandler) updateRequest(overrideSettings *map[string]interface{}, username, configVersion string) error {
	// version == -1 use default
	if configVersion == "-1" {
		return nil
	}
	// read config from db
	key := fmt.Sprintf("%s_%s", username, configVersion)
	data, err := a.configStore.Get(key, []string{datastore.KConfigVal})
	if err != nil {
		return err
	}
	val := data[datastore.KConfigVal].(string)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return nil
	}
	// default override_settings_restore_afterwards = true
	// remove sd_model_checkpoint and sd_vae
	delete(*overrideSettings, "sd_model_checkpoint")
	delete(*overrideSettings, "sd_vae")
	m["override_settings_restore_afterwards"] = true
	// priority request > db
	for k, v := range m {
		if _, ok := (*overrideSettings)[k]; ok {
			continue
		} else {
			(*overrideSettings)[k] = v
		}
	}
	return nil
}

// ListModels list model, not support
// (GET /models)
func (a *AgentHandler) ListModels(c *gin.Context) {
	c.String(http.StatusNotFound, "api not support")
}

// RegisterModel register model, not support
// (POST /models)
func (a *AgentHandler) RegisterModel(c *gin.Context) {
	c.String(http.StatusNotFound, "api not support")
}

// DeleteModel delete model, not support
// (DELETE /models/{model_name})
func (a *AgentHandler) DeleteModel(c *gin.Context, modelName string) {
	c.String(http.StatusNotFound, "api not support")
}

// GetModel get model info, not support
// (GET /models/{model_name})
func (a *AgentHandler) GetModel(c *gin.Context, modelName string) {
	c.String(http.StatusNotFound, "api not support")
}

// UpdateModel update model, not support
// (PUT /models/{model_name})
func (a *AgentHandler) UpdateModel(c *gin.Context, modelName string) {
	c.String(http.StatusNotFound, "api not support")
}

// CancelTask cancel predict task, not support
// (GET /tasks/{taskId}/cancellation)
func (a *AgentHandler) CancelTask(c *gin.Context, taskId string) {
	c.String(http.StatusNotFound, "api not support")
}

// GetTaskProgress get predict progress, not support
// (GET /tasks/{taskId}/progress)
func (a *AgentHandler) GetTaskProgress(c *gin.Context, taskId string) {
	c.String(http.StatusNotFound, "api not support")
}

// GetTaskResult get predict result, not support
// (GET /tasks/{taskId}/result)
func (a *AgentHandler) GetTaskResult(c *gin.Context, taskId string) {
	c.String(http.StatusNotFound, "api not support")
}

// UpdateOptions update config options
// (POST /options)
func (a *AgentHandler) UpdateOptions(c *gin.Context) {
	c.String(http.StatusNotFound, "api not support")
}

// Login user login
// (POST /login)
func (p *AgentHandler) Login(c *gin.Context) {
	c.String(http.StatusNotFound, "api not support")
}