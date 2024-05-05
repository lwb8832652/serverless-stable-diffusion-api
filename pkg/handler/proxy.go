package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/client"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/concurrency"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/config"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/datastore"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/models"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/module"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/utils"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

const DEFAULT_USER = "default"

type ProxyHandler struct {
	userStore     datastore.Datastore
	taskStore     datastore.Datastore
	modelStore    datastore.Datastore
	httpClient    *http.Client // the http client
	configStore   datastore.Datastore
	functionStore datastore.Datastore
}

func NewProxyHandler(taskStore datastore.Datastore,
	modelStore datastore.Datastore, userStore datastore.Datastore,
	configStore datastore.Datastore, functionStore datastore.Datastore) *ProxyHandler {
	return &ProxyHandler{
		taskStore:     taskStore,
		modelStore:    modelStore,
		httpClient:    &http.Client{},
		userStore:     userStore,
		configStore:   configStore,
		functionStore: functionStore,
	}
}

// Login user login
// (POST /login)
func (p *ProxyHandler) Login(c *gin.Context) {
	request := new(models.UserLoginRequest)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	token, expired, ok := module.UserManagerGlobal.VerifyUserValid(request.UserName, request.Password)
	if !ok {
		c.JSON(http.StatusGone, models.UserLoginResponse{
			Message: utils.String("login fail"),
		})
	} else {
		// update db
		p.userStore.Update(request.UserName, map[string]interface{}{
			datastore.KUserSession:          token,
			datastore.KUserSessionValidTime: fmt.Sprintf("%d", expired),
			datastore.KUserModifyTime:       fmt.Sprintf("%d", utils.TimestampS()),
		})
		c.JSON(http.StatusOK, models.UserLoginResponse{
			UserName: request.UserName,
			Token:    token,
			Message:  utils.String("login success"),
		})
	}
}

// Restart restart webui api server
// (POST /restart)
func (p *ProxyHandler) Restart(c *gin.Context) {
	if config.ConfigGlobal.IsServerTypeMatch(config.PROXY) {
		//retransmission to control
		target := config.ConfigGlobal.Downstream
		remote, err := url.Parse(target)
		if err != nil {
			panic(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(remote)
		proxy.Director = func(req *http.Request) {
			req.Header = c.Request.Header
			req.Host = remote.Host
			req.URL.Scheme = remote.Scheme
			req.URL.Host = remote.Host
		}
		proxy.ServeHTTP(c.Writer, c.Request)
	} else if config.ConfigGlobal.IsServerTypeMatch(config.CONTROL) {
		// update agent env
		err := module.FuncManagerGlobal.UpdateAllFunctionEnv()
		if err != nil {
			handleError(c, http.StatusInternalServerError, "update function env error")
		}
		c.JSON(http.StatusOK, gin.H{"message": "success"})
	} else {
		c.JSON(http.StatusNotFound, gin.H{"message": "not support"})
	}
}

// ListSdFunc get sdapi function
// (GET /list/sdapi/functions)
func (p *ProxyHandler) ListSdFunc(c *gin.Context) {
	if datas, err := p.functionStore.ListAll([]string{datastore.KModelServiceFunctionName}); err != nil {
		c.JSON(http.StatusInternalServerError, models.ListSDFunctionResponse{
			Status: utils.String("fail"),
			ErrMsg: utils.String(err.Error()),
		})
	} else {
		funcList := make([]map[string]interface{}, 0, len(datas))
		if datas != nil {
			for model, data := range datas {
				funcList = append(funcList, map[string]interface{}{
					"functionName": data[datastore.KModelServiceFunctionName].(string),
					"model":        model,
				})
			}
		}
		c.JSON(http.StatusOK, models.ListSDFunctionResponse{
			Status:    utils.String("success"),
			Functions: &funcList,
		})
	}
}

// BatchUpdateResource update sd function resource by batch, Supports a specified list of functions, or all
// (POST /batch_update_sd_resource)
func (p *ProxyHandler) BatchUpdateResource(c *gin.Context) {
	request := new(models.BatchUpdateSdResourceRequest)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, err.Error())
		return
	}
	// get request relevant function
	funcDatas, err := getFunctionDatas(p.functionStore, request)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"status": "fail",
			"errMsg": err.Error()})
		return
	}
	// update fc
	_, fail, errs := module.FuncManagerGlobal.UpdateFunctionResource(funcDatas)
	// response
	if len(fail) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "success"})
	} else {
		c.JSON(http.StatusInternalServerError, models.BatchUpdateSdResourceResponse{
			Status:       utils.String("fail"),
			FailFuncList: &fail,
			ErrMsg:       utils.String(strings.Join(errs, "|")),
		})
	}
}

// CancelTask predict task
// (POST /tasks/{taskId}/cancellation)
func (p *ProxyHandler) CancelTask(c *gin.Context, taskId string) {
	if err := p.taskStore.Update(taskId, map[string]interface{}{
		datastore.KTaskCancel: int64(config.CANCEL_VALID),
	}); err != nil {
		handleError(c, http.StatusInternalServerError, "update task cancel error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success"})
}

// GetTaskResult  get predict progress
// (GET /tasks/{taskId}/result)
func (p *ProxyHandler) GetTaskResult(c *gin.Context, taskId string) {
	result, err := p.getTaskResult(taskId)
	if err != nil {
		handleError(c, http.StatusNotFound, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// ListModels list model
// (GET /models)
func (p *ProxyHandler) ListModels(c *gin.Context) {
	if config.ConfigGlobal.UseLocalModel() {
		// get from local disk
		ret := make([]*models.ModelAttributes, 0)
		// sdModel
		path := fmt.Sprintf("%s/models/%s", config.ConfigGlobal.SdPath, "Stable-diffusion")
		ret = append(ret, listModelFile(path, config.SD_MODEL)...)
		// sdVae
		path = fmt.Sprintf("%s/models/%s", config.ConfigGlobal.SdPath, "VAE")
		ret = append(ret, listModelFile(path, config.SD_VAE)...)
		// lora
		path = fmt.Sprintf("%s/models/%s", config.ConfigGlobal.SdPath, "Lora")
		ret = append(ret, listModelFile(path, config.LORA_MODEL)...)
		// controlNet
		path = fmt.Sprintf("%s/models/%s", config.ConfigGlobal.SdPath, "ControlNet")
		ret = append(ret, listModelFile(path, config.CONTORLNET_MODEL)...)
		c.JSON(http.StatusOK, ret)
	} else {
		// get from db
		val, err := p.modelStore.ListAll([]string{datastore.KModelType, datastore.KModelName,
			datastore.KModelOssPath, datastore.KModelEtag, datastore.KModelStatus, datastore.KModelCreateTime,
			datastore.KModelModifyTime})
		if err != nil {
			handleError(c, http.StatusInternalServerError, "read model from db error")
			return
		}
		c.JSON(http.StatusOK, convertToModelResponse(val))
	}

}

// RegisterModel upload model
// (POST /models)
func (p *ProxyHandler) RegisterModel(c *gin.Context) {
	if config.ConfigGlobal.UseLocalModel() {
		c.String(http.StatusNotFound, "useLocalModel=yes not support")
		return
	}
	request := new(models.RegisterModelJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// check models exist or not
	data, err := p.modelStore.Get(request.Name, []string{datastore.KModelName,
		datastore.KModelEtag, datastore.KModelOssPath, datastore.KModelStatus})
	if err != nil {
		handleError(c, http.StatusInternalServerError, "read models db error")
		return
	}

	// models existed
	if data != nil && len(data) != 0 && data[datastore.KModelStatus].(string) != config.MODEL_DELETE && data[datastore.KModelEtag].(string) == request.Etag &&
		data[datastore.KModelOssPath].(string) == request.OssPath {
		c.JSON(http.StatusOK, gin.H{"message": "models existed"})
		return
	}
	// from oss download model to local
	localFile, err := downloadModelsFromOss(request.Type, request.OssPath, request.Name)
	if err != nil {
		handleError(c, http.StatusInternalServerError, fmt.Sprintf("please check oss model valid, "+
			"err=%s", err.Error()))
		return
	}

	// update db
	data = map[string]interface{}{
		datastore.KModelType:       request.Type,
		datastore.KModelName:       request.Name,
		datastore.KModelOssPath:    request.OssPath,
		datastore.KModelEtag:       request.Etag,
		datastore.KModelLocalPath:  localFile,
		datastore.KModelStatus:     getModelsStatus(request.Type),
		datastore.KModelCreateTime: fmt.Sprintf("%d", utils.TimestampS()),
		datastore.KModelModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}
	p.modelStore.Put(request.Name, data)
	c.JSON(http.StatusOK, gin.H{"message": "register success"})
}

// DeleteModel delete model
// (DELETE /models/{model_name})
func (p *ProxyHandler) DeleteModel(c *gin.Context, modelName string) {
	if config.ConfigGlobal.UseLocalModel() {
		c.String(http.StatusNotFound, "useLocalModel=yes not support")
		return
	}
	// get local file path
	data, err := p.modelStore.Get(modelName, []string{datastore.KModelLocalPath, datastore.KModelStatus})
	if err != nil {
		handleError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if data == nil || len(data) == 0 || data[datastore.KModelStatus] == config.MODEL_DELETE {
		handleError(c, http.StatusInternalServerError, "model not exist")
		return
	}
	localFile := data[datastore.KModelLocalPath].(string)
	// delete nas models
	if ok, err := utils.DeleteLocalFile(localFile); !ok {
		handleError(c, http.StatusInternalServerError, err.Error())
		return
	}
	// model status set deleted
	if err := p.modelStore.Update(modelName, map[string]interface{}{
		datastore.KModelStatus:     config.MODEL_DELETE,
		datastore.KModelModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}); err != nil {
		handleError(c, http.StatusInternalServerError, "update model status error")
	} else {
		c.JSON(http.StatusOK, gin.H{"message": "delete success"})
	}
}

// GetModel get model info
// (GET /models/{model_name})
func (p *ProxyHandler) GetModel(c *gin.Context, modelName string) {
	if config.ConfigGlobal.UseLocalModel() {
		c.String(http.StatusNotFound, "useLocalModel=yes not support")
		return
	}
	data, err := p.modelStore.Get(modelName, []string{datastore.KModelType, datastore.KModelName,
		datastore.KModelOssPath, datastore.KModelEtag, datastore.KModelStatus, datastore.KModelCreateTime,
		datastore.KModelModifyTime})
	if err != nil {
		handleError(c, http.StatusInternalServerError, "get model info from db error")
		return
	}
	if data == nil || len(data) == 0 {
		handleError(c, http.StatusNotFound, config.NOTFOUND)
		return
	}
	c.JSON(http.StatusOK, convertToModelResponse(map[string]map[string]interface{}{
		modelName: data,
	}))

}

// UpdateModel update model
// (PUT /models/{model_name})
func (p *ProxyHandler) UpdateModel(c *gin.Context, modelName string) {
	if config.ConfigGlobal.UseLocalModel() {
		c.String(http.StatusNotFound, "useLocalModel=yes not support")
		return
	}
	request := new(models.UpdateModelJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// check models exist or not
	data, err := p.modelStore.Get(modelName, []string{datastore.KModelName,
		datastore.KModelEtag, datastore.KModelOssPath, datastore.KModelStatus})
	if err != nil {
		handleError(c, http.StatusInternalServerError, "read models db error")
		return
	}
	// models existed and not change
	if data != nil {
		if data[datastore.KModelStatus].(string) == config.MODEL_DELETE {
			handleError(c, http.StatusNotFound, "model not register, please register first")
			return
		} else if data[datastore.KModelEtag].(string) == request.Etag &&
			data[datastore.KModelOssPath].(string) == request.OssPath {
			c.JSON(http.StatusOK, gin.H{"message": "models existed and not change"})
			return
		}
	} else {
		handleError(c, http.StatusNotFound, "model not register, please register first")
		return
	}
	// from oss download nas
	if _, err := downloadModelsFromOss(request.Type, request.OssPath, request.Name); err != nil {
		handleError(c, http.StatusInternalServerError, fmt.Sprintf("please check oss model valid, "+
			"err=%s", err.Error()))
		return
	}
	// sdModel and sdVae enable env update
	if request.Type == config.SD_MODEL || request.Type == config.SD_VAE {
		if err := module.FuncManagerGlobal.UpdateFunctionEnv(request.Name); err != nil {
			handleError(c, http.StatusInternalServerError, config.MODELUPDATEFCERROR)
			return
		}
	}

	// update db
	data = map[string]interface{}{
		datastore.KModelType:       request.Type,
		datastore.KModelOssPath:    request.OssPath,
		datastore.KModelEtag:       request.Etag,
		datastore.KModelStatus:     getModelsStatus(request.Type),
		datastore.KModelModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}
	if err := p.modelStore.Update(modelName, data); err != nil {
		handleError(c, http.StatusInternalServerError, config.NOTFOUND)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success"})

}

// GetTaskProgress get predict progress
// (GET /tasks/{taskId}/progress)
func (p *ProxyHandler) GetTaskProgress(c *gin.Context, taskId string) {
	data, err := p.taskStore.Get(taskId, []string{datastore.KTaskIdColumnName, datastore.KTaskStatus,
		datastore.KTaskProgressColumnName})
	if err != nil || data == nil || len(data) == 0 {
		handleError(c, http.StatusNotFound, config.NOTFOUND)
		return
	}
	resp := new(models.TaskProgressResponse)
	if progress, ok := data[datastore.KTaskProgressColumnName]; ok {
		if err := json.Unmarshal([]byte(progress.(string)), resp); err != nil {
			handleError(c, http.StatusInternalServerError, config.NOTFOUND)
			return
		}
	}
	if status, ok := data[datastore.KTaskStatus]; ok && (status == config.TASK_FINISH || status == config.TASK_FAILED) {
		resp.Progress = 1
	} else if resp.Progress == 1 {
		// task finish need status == config.TASK_FINISH|config.TASK_FAILED
		resp.Progress = 0.99
	}
	resp.TaskId = taskId
	c.JSON(http.StatusOK, resp)
}

// ExtraImages image upcaling
// (POST /extra_images)
func (p *ProxyHandler) ExtraImages(c *gin.Context) {
	username := c.GetHeader(userKey)
	invokeType := c.GetHeader(requestType)
	if username == "" {
		if config.ConfigGlobal.EnableLogin() {
			handleError(c, http.StatusBadRequest, config.BADREQUEST)
			return
		} else {
			username = DEFAULT_USER
		}
	}
	request := new(models.ExtraImagesJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	// taskId
	taskId := c.GetHeader(taskKey)
	if taskId == "" {
		// init taskId
		taskId = utils.RandStr(taskIdLength)
	}
	c.Writer.Header().Set("taskId", taskId)

	endPoint := config.ConfigGlobal.Downstream
	var err error
	if config.ConfigGlobal.IsServerTypeMatch(config.CONTROL) {
		if endPoint = module.FuncManagerGlobal.GetLastInvokeEndpoint(request.StableDiffusionModel); endPoint == "" {
			handleError(c, http.StatusInternalServerError, "not found valid endpoint")
			return
		}
	}

	// write db
	if err := p.taskStore.Put(taskId, map[string]interface{}{
		datastore.KTaskIdColumnName: taskId,
		datastore.KTaskUser:         username,
		datastore.KTaskStatus:       config.TASK_QUEUE,
		datastore.KTaskCreateTime:   fmt.Sprintf("%d", utils.TimestampS()),
	}); err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorf("put db err=%s", err.Error())
		c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
			TaskId:  taskId,
			Status:  config.TASK_FAILED,
			Message: utils.String(config.INTERNALERROR),
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.HTTPTIMEOUT)
	defer cancel()
	// get client by endPoint
	client := client.ManagerClientGlobal.GetClient(endPoint)
	// async request
	resp, err := client.ExtraImages(ctx, *request, func(ctx context.Context, req *http.Request) error {
		req.Header.Add(userKey, username)
		req.Header.Add(taskKey, taskId)
		if isAsync(invokeType) {
			req.Header.Add(FcAsyncKey, "Async")
		}
		return nil
	})
	if err != nil || (resp.StatusCode != syncSuccessCode && resp.StatusCode != asyncSuccessCode) {
		handleRespError(c, err, resp, taskId)
	} else {
		c.JSON(http.StatusOK, models.SubmitTaskResponse{
			TaskId: taskId,
			Status: func() string {
				if resp.StatusCode == syncSuccessCode {
					return config.TASK_FINISH
				}
				if resp.StatusCode == asyncSuccessCode {
					return config.TASK_QUEUE
				}
				return config.TASK_FAILED
			}(),
			OssUrl: extraOssUrl(resp),
		})
	}
}

// Txt2Img txt to img predict
// (POST /txt2img)
func (p *ProxyHandler) Txt2Img(c *gin.Context) {
	username := c.GetHeader(userKey)
	//invokeType := c.GetHeader(requestType)
	if username == "" {
		if config.ConfigGlobal.EnableLogin() {
			handleError(c, http.StatusBadRequest, config.BADREQUEST)
			return
		} else {
			username = DEFAULT_USER
		}
	}
	request := new(models.Txt2ImgJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	if !checkSdModelValid(request.StableDiffusionModel) {
		handleError(c, http.StatusBadRequest, "stable_diffusion_model val not valid, please set valid val")
		return
	}

	// taskId
	taskId := request.ForceTaskId
	if taskId == "" {
		// init taskId
		taskId = utils.RandStr(taskIdLength)
		request.ForceTaskId = taskId
	}
	c.Writer.Header().Set("taskId", taskId)
	if config.ConfigGlobal.IsServerTypeMatch(config.PROXY) {
		// check request valid: sdModel and sdVae exist
		if existed := p.checkModelExist(request.StableDiffusionModel); !existed {
			handleError(c, http.StatusNotFound, "model not found, please check request")
			return
		}
		// write db
		if err := p.taskStore.Put(taskId, map[string]interface{}{
			datastore.KTaskIdColumnName: taskId,
			datastore.KTaskUser:         username,
			datastore.KTaskStatus:       config.TASK_QUEUE,
			datastore.KTaskCancel:       int64(config.CANCEL_INIT),
			datastore.KTaskCreateTime:   fmt.Sprintf("%d", utils.TimestampS()),
		}); err != nil {
			logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorf("put db err=%s", err.Error())
			c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
				TaskId:  taskId,
				Status:  config.TASK_FAILED,
				Message: utils.String(config.OTSPUTERROR),
			})
			return
		}
	}

	// preprocess request ossPath image to base64
	if err := preprocessRequest(request); err != nil {
		// update task status
		p.taskStore.Update(taskId, map[string]interface{}{
			datastore.KTaskStatus:     config.TASK_FAILED,
			datastore.KTaskCode:       int64(requestFail),
			datastore.KTaskModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
		})
		handleError(c, http.StatusBadRequest, err.Error())
		return
	}

	// update request OverrideSettings
	if request.OverrideSettings == nil {
		overrideSettings := make(map[string]interface{})
		request.OverrideSettings = &overrideSettings
	}
	configVer := c.GetHeader(versionKey)
	if err := p.updateOverrideSettingsRequest(request.OverrideSettings, username, configVer,
		request.StableDiffusionModel, request.SdVae); err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorf("update OverrideSettings err=%s", err.Error())
		handleError(c, http.StatusInternalServerError, "please check config")
		return
	}

	// default OverrideSettingsRestoreAfterwards = true
	request.OverrideSettingsRestoreAfterwards = utils.Bool(false)

	body, err := json.Marshal(request)
	if err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorln("request to json err=", err.Error())
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}

	// predict task
	images, err := p.predictTask(username, taskId, config.TXT2IMG, body)
	if err != nil {
		//logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorln(err.Error())
		c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
			TaskId:  taskId,
			Status:  config.TASK_FAILED,
			Message: utils.String(""),
		})
		return
	}
	if ossUrl, err := module.OssGlobal.GetUrl(images); err != nil {
		logrus.Error("get oss url error")
		c.JSON(http.StatusInternalServerError, gin.H{
			"message": "get oss url error",
		})
	} else {
		c.JSON(http.StatusOK, models.SubmitTaskResponse{
			TaskId: taskId,
			Status: config.TASK_FINISH,
			OssUrl: &ossUrl,
		})
	}
}

func (p *ProxyHandler) predictTask(user, taskId, path string, body []byte) ([]string, error) {
	url := fmt.Sprintf("%s%s", config.ConfigGlobal.SdUrlPrefix, path)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	body, err = io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var result *models.Txt2ImgResult

	if err := json.Unmarshal(body, &result); err != nil {
		//logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorln(err.Error())
		return nil, err
	}
	if result == nil {
		if err := p.taskStore.Update(taskId, map[string]interface{}{
			datastore.KTaskCode:       int64(resp.StatusCode),
			datastore.KTaskStatus:     config.TASK_FAILED,
			datastore.KTaskInfo:       string(body),
			datastore.KTaskModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
		}); err != nil {
			logrus.WithFields(logrus.Fields{"taskId": taskId}).Println(err.Error())
			return nil, err
		}
		return nil, errors.New("predict fail")
	}
	if result.Parameters != nil {
		result.Parameters["alwayson_scripts"] = ""
	}
	params, err := json.Marshal(result.Parameters)
	if err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Println("json:", err.Error())
	}
	var images []string
	var status string
	var errMeg error
	if resp.StatusCode == requestOk {
		count := len(result.Images)
		for i := 1; i <= count; i++ {
			// upload image to oss
			ossPath := fmt.Sprintf("images/%s/%s_%d.png", user, taskId, i)
			if err := uploadImages(&ossPath, &result.Images[i-1]); err != nil {
				return nil, fmt.Errorf("output image err=%s", err.Error())
			}

			images = append(images, ossPath)
		}
		status = config.TASK_FINISH
	} else {
		status = config.TASK_FAILED
		errMeg = errors.New("predict error")
	}
	if err := p.taskStore.Update(taskId, map[string]interface{}{
		datastore.KTaskCode:       int64(resp.StatusCode),
		datastore.KTaskStatus:     status,
		datastore.KTaskImage:      strings.Join(images, ","),
		datastore.KTaskParams:     string(params),
		datastore.KTaskInfo:       result.Info,
		datastore.KTaskModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}); err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Errorln(err.Error())
		return nil, err
	}
	return images, errMeg
}

// deal ossImg to base64
func preprocessRequest(req any) error {
	switch req.(type) {
	case *models.ExtraImagesJSONRequestBody:
		request := req.(*models.ExtraImagesJSONRequestBody)
		if request.Image != "" {
			if isImgPath(request.Image) {

				base64, err := module.OssGlobal.DownloadFileToBase64(request.Image)
				if err != nil {
					return err
				}
				request.Image = *base64
			}
		}
	case *models.Txt2ImgJSONRequestBody:
		request := req.(*models.Txt2ImgJSONRequestBody)
		if request.AlwaysonScripts != nil {
			return updateControlNet(request.AlwaysonScripts)
		}
	case *models.Img2ImgJSONRequestBody:
		request := req.(*models.Img2ImgJSONRequestBody)
		// init images: ossPath to base64Str
		for i, str := range *request.InitImages {
			if !isImgPath(str) {
				continue
			}
			base64, err := module.OssGlobal.DownloadFileToBase64(str)
			if err != nil {
				return err
			}
			(*request.InitImages)[i] = *base64
		}

		// mask images: ossPath to base64St
		if request.Mask != nil && isImgPath(*request.Mask) {
			base64, err := module.OssGlobal.DownloadFileToBase64(*request.Mask)
			if err != nil {
				return err
			}
			*request.Mask = *base64
		}

		// controlNet images: ossPath to base64Str
		if request.AlwaysonScripts != nil {
			return updateControlNet(request.AlwaysonScripts)
		}
	}
	return nil
}

func updateControlNet(alwaysonScripts *map[string]interface{}) error {
	*alwaysonScripts = parseMap(*alwaysonScripts, "", "", nil)
	return nil
}

func (p *ProxyHandler) updateOverrideSettingsRequest(overrideSettings *map[string]interface{},
	username, configVersion, sdModel string, sdVae *string) error {
	//if config.ConfigGlobal.GetFlexMode() == config.MultiFunc {
	//	// remove sd_model_checkpoint and sd_vae
	//	delete(*overrideSettings, "sd_model_checkpoint")
	//	(*overrideSettings)["sd_vae"] = sdVae
	//} else {
	(*overrideSettings)["sd_model_checkpoint"] = sdModel
	if sdVae != nil {
		(*overrideSettings)["sd_vae"] = sdVae
	} else {
		(*overrideSettings)["sd_vae"] = "None"
	}
	//}
	// version == -1 use default
	if configVersion == "-1" {
		return nil
	}
	// read config from db
	key := fmt.Sprintf("%s_%s", username, configVersion)
	data, err := p.configStore.Get(key, []string{datastore.KConfigVal})
	if err != nil {
		return err
	}
	// no user config, user default
	if data == nil || len(data) == 0 {
		return nil
	}
	val := data[datastore.KConfigVal].(string)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return nil
	}
	// priority request > db
	for k, v := range m {
		if _, ok := (*overrideSettings)[k]; !ok {
			(*overrideSettings)[k] = v
		}
	}
	return nil
}

// Img2Img img to img predict
// (POST /img2img)
func (p *ProxyHandler) Img2Img(c *gin.Context) {
	username := c.GetHeader(userKey)
	invokeType := c.GetHeader(requestType)
	if username == "" {
		if config.ConfigGlobal.EnableLogin() {
			handleError(c, http.StatusBadRequest, config.BADREQUEST)
			return
		} else {
			username = DEFAULT_USER
		}
	}
	request := new(models.Img2ImgJSONRequestBody)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	if !checkSdModelValid(request.StableDiffusionModel) {
		handleError(c, http.StatusBadRequest, "stable_diffusion_model val not valid, please set valid val")
		return
	}
	// taskId
	taskId := c.GetHeader(taskKey)
	if taskId == "" {
		// init taskId
		taskId = utils.RandStr(taskIdLength)
	}
	c.Writer.Header().Set("taskId", taskId)

	endPoint := config.ConfigGlobal.Downstream
	var err error
	version := c.GetHeader(versionKey)
	if config.ConfigGlobal.IsServerTypeMatch(config.CONTROL) {
		// get endPoint
		sdModel := request.StableDiffusionModel
		c.Writer.Header().Set("model", sdModel)
		// wait to valid
		if concurrency.ConCurrencyGlobal.WaitToValid(sdModel) {
			// cold start
			logrus.WithFields(logrus.Fields{"taskId": taskId}).Infof("sd %s cold start ....", sdModel)
			defer concurrency.ConCurrencyGlobal.DecColdNum(sdModel, taskId)
		}
		defer concurrency.ConCurrencyGlobal.DoneTask(sdModel, taskId)
		endPoint, err = module.FuncManagerGlobal.GetEndpoint(sdModel)
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
				TaskId:  taskId,
				Status:  config.TASK_FAILED,
				Message: utils.String(err.Error()),
			})
			return
		}
	}
	if config.ConfigGlobal.IsServerTypeMatch(config.PROXY) {
		// check request valid: sdModel and sdVae exist
		if existed := p.checkModelExist(request.StableDiffusionModel); !existed {
			handleError(c, http.StatusNotFound, "model not found, please check request")
			return
		}
		// write db
		if err := p.taskStore.Put(taskId, map[string]interface{}{
			datastore.KTaskIdColumnName: taskId,
			datastore.KTaskUser:         username,
			datastore.KTaskStatus:       config.TASK_QUEUE,
			datastore.KTaskCancel:       int64(config.CANCEL_INIT),
			datastore.KTaskCreateTime:   fmt.Sprintf("%d", utils.TimestampS()),
		}); err != nil {
			logrus.WithFields(logrus.Fields{"taskId": taskId}).Error("[Error] put db err=", err.Error())
			c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
				TaskId:  taskId,
				Status:  config.TASK_FAILED,
				Message: utils.String(config.OTSPUTERROR),
			})
			return
		}

		// get user current config version
		userItem, err := p.userStore.Get(username, []string{datastore.KUserConfigVer})
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
				TaskId:  taskId,
				Status:  config.TASK_FAILED,
				Message: utils.String(config.OTSGETERROR),
			})
			logrus.WithFields(logrus.Fields{"taskId": taskId}).Error("get config version err=", err.Error())
			return
		}
		version = func() string {
			if version, ok := userItem[datastore.KUserConfigVer]; !ok {
				return "-1"
			} else {
				return version.(string)
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), config.HTTPTIMEOUT)
	defer cancel()
	// get client by endPoint
	client := client.ManagerClientGlobal.GetClient(endPoint)
	// async request
	resp, err := client.Img2Img(ctx, *request, func(ctx context.Context, req *http.Request) error {
		req.Header.Add(userKey, username)
		req.Header.Add(taskKey, taskId)
		req.Header.Add(versionKey, version)
		if isAsync(invokeType) {
			req.Header.Add(FcAsyncKey, "Async")
		}
		return nil
	})
	if err != nil || (resp.StatusCode != syncSuccessCode && resp.StatusCode != asyncSuccessCode) {
		handleRespError(c, err, resp, taskId)
	} else {
		c.JSON(http.StatusOK, models.SubmitTaskResponse{
			TaskId: taskId,
			Status: func() string {
				if resp.StatusCode == syncSuccessCode {
					return config.TASK_FINISH
				}
				if resp.StatusCode == asyncSuccessCode {
					return config.TASK_QUEUE
				}
				return config.TASK_FAILED
			}(),
			OssUrl: extraOssUrl(resp),
		})
	}
}

// DelSDFunc delete sd function
// (POST /del/sd/functions)
func (p *ProxyHandler) DelSDFunc(c *gin.Context) {
	username := c.GetHeader(userKey)
	if username == "" {
		if config.ConfigGlobal.EnableLogin() {
			handleError(c, http.StatusBadRequest, config.BADREQUEST)
			return
		} else {
			username = DEFAULT_USER
		}
	}
	request := new(models.DelSDFunctionRequest)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	logrus.Info(*request.Functions)
	if fails, errs := module.FuncManagerGlobal.DeleteFunction(*request.Functions); fails != nil && len(fails) > 0 {
		failFuncs := make([]map[string]interface{}, 0, len(fails))
		for i, _ := range fails {
			failFuncs = append(failFuncs, map[string]interface{}{
				"functionName": fails[i],
				"err":          errs[i],
			})
		}
		c.JSON(http.StatusInternalServerError, models.DelSDFunctionResponse{
			Status: utils.String("fail"),
			Fails:  &failFuncs,
		})
	} else {
		c.JSON(http.StatusOK, models.DelSDFunctionResponse{
			Status: utils.String("success"),
		})
	}

}

// UpdateOptions update config options
// (POST /options)
func (p *ProxyHandler) UpdateOptions(c *gin.Context) {
	username := c.GetHeader(userKey)
	if username == "" {
		if config.ConfigGlobal.EnableLogin() {
			handleError(c, http.StatusBadRequest, config.BADREQUEST)
			return
		} else {
			username = DEFAULT_USER
		}
	}
	request := new(models.OptionRequest)
	if err := getBindResult(c, request); err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	configStr, err := json.Marshal(request.Data)
	if err != nil {
		handleError(c, http.StatusBadRequest, config.BADREQUEST)
		return
	}
	version := fmt.Sprintf("%d", utils.TimestampS())
	key := fmt.Sprintf("%s_%s", username, version)
	if err := p.configStore.Put(key, map[string]interface{}{
		datastore.KConfigVal:      string(configStr),
		datastore.KConfigVer:      version,
		datastore.KUserModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}); err != nil {
		handleError(c, http.StatusInternalServerError, "update db error")
		return
	}
	if err := p.userStore.Update(username, map[string]interface{}{
		datastore.KUserConfigVer:  version,
		datastore.KUserModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	}); err != nil {
		if !config.ConfigGlobal.EnableLogin() {
			// if username not existed add user
			if err = p.userStore.Put(username, map[string]interface{}{
				datastore.KUserConfigVer:  version,
				datastore.KUserCreateTime: fmt.Sprintf("%d", utils.TimestampS()),
				datastore.KUserModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
			}); err == nil {
				c.JSON(http.StatusOK, gin.H{"message": "success"})
				return
			}

		}
		handleError(c, http.StatusInternalServerError, "update db error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success"})
}

func (p *ProxyHandler) getTaskResult(taskId string) (*models.TaskResultResponse, error) {
	result := &models.TaskResultResponse{
		TaskId:     taskId,
		Status:     config.TASK_QUEUE,
		Parameters: new(map[string]interface{}),
		Info:       new(map[string]interface{}),
		Images:     new([]string),
		OssUrl:     new([]string),
	}
	data, err := p.taskStore.Get(taskId, []string{datastore.KTaskStatus, datastore.KTaskImage, datastore.KTaskInfo,
		datastore.KTaskParams, datastore.KTaskCode})
	if err != nil || data == nil || len(data) == 0 {
		return nil, errors.New("not found")
	}

	// not success
	if status, ok := data[datastore.KTaskStatus]; ok && (status != config.TASK_FINISH) {
		result.Status = status.(string)
		return result, nil
	} else if ok {
		result.Status = config.TASK_FINISH
	}

	if code, ok := data[datastore.KTaskCode]; ok && code.(int64) != requestOk {
		result.Status = config.TASK_FAILED
		return result, nil
	} else if !ok {
		return nil, fmt.Errorf("task:%s predict fail", taskId)
	}

	// images
	*result.Images = strings.Split(data[datastore.KTaskImage].(string), ",")
	// params
	paramsStr := data[datastore.KTaskParams].(string)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(paramsStr), &m); err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Println("Unmarshal params error=", err.Error())
	}
	*result.Parameters = m
	// info
	var mm map[string]interface{}
	infoStr := data[datastore.KTaskInfo].(string)
	if err := json.Unmarshal([]byte(infoStr), &mm); err != nil {
		logrus.WithFields(logrus.Fields{"taskId": taskId}).Println("Unmarshal Info error=", err.Error())
	}
	*result.Info = mm
	if ossUrl, err := module.OssGlobal.GetUrl(*result.Images); err == nil {
		*result.OssUrl = ossUrl
	} else {
		logrus.Warn("get oss url error")
	}
	return result, nil
}

func (p *ProxyHandler) checkModelExist(sdModel string) bool {
	// mount nas && check
	if !utils.FileExists(config.ConfigGlobal.SdPath) {
		return true
	}
	models := [][]string{{config.SD_MODEL, sdModel}}
	//// remove sdVae = None || Automatic
	//if sdVae != "None" && sdVae != "Automatic" {
	//	models = append(models, []string{config.MODEL_SD_VAE, sdVae})
	//}
	for _, model := range models {
		// check local existed
		switch model[0] {
		case config.SD_MODEL:
			sdModelPath := fmt.Sprintf("%s/models/%s/%s", config.ConfigGlobal.SdPath, "Stable-diffusion", sdModel)
			if !utils.FileExists(sdModelPath) {
				// list check image models
				path := fmt.Sprintf("%s/models/%s", config.ConfigGlobal.SdPath, "Stable-diffusion")
				tmp := utils.ListFile(path)
				for _, one := range tmp {
					if one == sdModel {
						return true
					}
				}
				return false
			}
			//case config.MODEL_SD_VAE:
			//	sdVaePath := fmt.Sprintf("%s/models/%s/%s", config.ConfigGlobal.SdPath, "VAE", sdVae)
			//	if !utils.FileExists(sdVaePath) {
			//		// list check image models
			//		path := fmt.Sprintf("%s/models/%s", config.ConfigGlobal.SdPath, "VAE")
			//		tmp := utils.ListFile(path)
			//		for _, one := range tmp {
			//			if one == sdVae {
			//				return true
			//			}
			//		}
			//		return false
			//	}
		}
	}
	return true
}

func convertToModelResponse(datas map[string]map[string]interface{}) []*models.ModelAttributes {
	ret := make([]*models.ModelAttributes, 0, len(datas))
	for _, data := range datas {
		registeredTime := data[datastore.KModelCreateTime].(string)
		modifyTime := data[datastore.KModelModifyTime].(string)
		ret = append(ret, &models.ModelAttributes{
			Type:                 data[datastore.KModelType].(string),
			Name:                 data[datastore.KModelName].(string),
			OssPath:              data[datastore.KModelOssPath].(string),
			Etag:                 data[datastore.KModelEtag].(string),
			Status:               data[datastore.KModelStatus].(string),
			RegisteredTime:       &registeredTime,
			LastModificationTime: &modifyTime,
		})
	}
	return ret
}

func getModelsStatus(modelType string) string {
	switch modelType {
	case config.SD_MODEL, config.SD_VAE:
		return config.MODEL_LOADED
	default:
		return config.MODEL_LOADED
	}
}

func ApiAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path != "/login" {
			tokenString := c.Request.Header.Get("Token")
			userName, ok := module.UserManagerGlobal.VerifySessionValid(tokenString)
			if !ok {
				c.JSON(http.StatusGone, gin.H{"message": "please login first or login expired"})
				c.Abort()
			}
			c.Request.Header.Set("userName", userName)
		}
	}
}

func isAsync(invokeType string) bool {
	// control server default sync
	if config.ConfigGlobal.GetFlexMode() == config.MultiFunc && config.ConfigGlobal.IsServerTypeMatch(config.CONTROL) {
		return false
	}
	if invokeType == "async" {
		return true
	}
	return false
}

func (p *ProxyHandler) NoRouterHandler(c *gin.Context) {
	username := c.GetHeader(userKey)
	if username == "" {
		if config.ConfigGlobal.EnableLogin() {
			handleError(c, http.StatusBadRequest, config.BADREQUEST)
			return
		} else {
			username = DEFAULT_USER
		}
	}
	taskId := ""
	if isTask := c.GetHeader("Task-Flag"); isTask == "true" || isAsync(c.GetHeader(requestType)) {
		// taskId
		taskId = c.GetHeader(taskKey)
		if taskId == "" {
			// init taskId
			taskId = utils.RandStr(taskIdLength)
		}
		c.Writer.Header().Set("taskId", taskId)
	}
	// control
	endPoint := config.ConfigGlobal.Downstream
	// get endPoint
	sdModel := ""
	body, _ := io.ReadAll(c.Request.Body)
	defer c.Request.Body.Close()
	if config.ConfigGlobal.IsServerTypeMatch(config.CONTROL) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodDelete {
			// extra body
			request := make(map[string]interface{})
			err := json.Unmarshal(body, &request)
			if err != nil {
				c.String(http.StatusBadRequest, err.Error())
				return
			}
			if sd, ok := request["StableDiffusionModel"]; ok {
				sdModel = sd.(string)
			}
		}
		c.Writer.Header().Set("model", sdModel)
		// wait to valid
		if concurrency.ConCurrencyGlobal.WaitToValid(sdModel) {
			// cold start
			logrus.WithFields(logrus.Fields{"taskId": taskId}).Infof("sd %s cold start ....", sdModel)
			defer concurrency.ConCurrencyGlobal.DecColdNum(sdModel, taskId)
		}
		defer concurrency.ConCurrencyGlobal.DoneTask(sdModel, taskId)
		var err error
		if sdModel == "" {
			endPoint = module.FuncManagerGlobal.GetLastInvokeEndpoint(&sdModel)
		} else {
			endPoint, err = module.FuncManagerGlobal.GetEndpoint(sdModel)
		}

		if err != nil {
			c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
				TaskId:  taskId,
				Status:  config.TASK_FAILED,
				Message: utils.String(err.Error()),
			})
			return
		}
	}
	// proxy
	if config.ConfigGlobal.IsServerTypeMatch(config.PROXY) {
		// check request valid: sdModel and sdVae exist
		if sdModel != "" {
			if existed := p.checkModelExist(sdModel); !existed {
				handleError(c, http.StatusNotFound, "model not found, please check request")
				return
			}
		}
		if taskId != "" {
			// write db
			if err := p.taskStore.Put(taskId, map[string]interface{}{
				datastore.KTaskIdColumnName: taskId,
				datastore.KTaskUser:         username,
				datastore.KTaskStatus:       config.TASK_QUEUE,
				datastore.KTaskCancel:       int64(config.CANCEL_INIT),
				datastore.KTaskCreateTime:   fmt.Sprintf("%d", utils.TimestampS()),
			}); err != nil {
				logrus.WithFields(logrus.Fields{"taskId": taskId}).Error("[Error] put db err=", err.Error())
				c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
					TaskId:  taskId,
					Status:  config.TASK_FAILED,
					Message: utils.String(err.Error()),
				})
				return
			}
			c.Header("taskId", taskId)
		}
	}
	req, err := http.NewRequest(c.Request.Method, fmt.Sprintf("%s%s", endPoint, c.Request.URL.String()),
		bytes.NewReader(body))
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	req.Header = c.Request.Header
	if taskId != "" {
		req.Header.Set("taskId", taskId)
	}
	if isAsync(c.GetHeader(requestType)) {
		req.Header.Set(FcAsyncKey, "Async")
	}
	req.Header.Set(userKey, username)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	if isAsync(c.GetHeader(requestType)) {
		if err != nil || (resp.StatusCode != syncSuccessCode && resp.StatusCode != asyncSuccessCode) {
			c.JSON(http.StatusInternalServerError, models.SubmitTaskResponse{
				TaskId:  taskId,
				Status:  config.TASK_FAILED,
				Message: utils.String(config.INTERNALERROR),
			})
		} else {
			c.JSON(http.StatusOK, models.SubmitTaskResponse{
				TaskId: taskId,
				Status: func() string {
					if resp.StatusCode == syncSuccessCode {
						return config.TASK_FINISH
					}
					if resp.StatusCode == asyncSuccessCode {
						return config.TASK_QUEUE
					}
					return config.TASK_FAILED
				}(),
			})
		}
	} else {
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Data(http.StatusOK, resp.Header.Get("Content-Type"), body)
	}
}
