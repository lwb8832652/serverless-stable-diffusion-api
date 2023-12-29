package module

import (
	"encoding/json"
	"errors"
	"fmt"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	fc3 "github.com/alibabacloud-go/fc-20230330/client"
	fc "github.com/alibabacloud-go/fc-open-20210406/v2/client"
	fcService "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/config"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/datastore"
	"github.com/devsapp/serverless-stable-diffusion-api/pkg/utils"
	"github.com/sirupsen/logrus"
	"sync"
	"time"
)

const (
	RETRY_INTERVALMS = time.Duration(10) * time.Millisecond
)

type SdModels struct {
	sdModel  string
	sdVae    string
	endpoint string
}

// FuncResource Fc resource
type FuncResource struct {
	Image         string             `json:"image"`
	CPU           float32            `json:"cpu"`
	GpuMemorySize int32              `json:"gpuMemorySize"`
	InstanceType  string             `json:"InstanceType"`
	MemorySize    int32              `json:"memorySize"`
	Timeout       int32              `json:"timeout"`
	Env           map[string]*string `json:"env"`
}

var FuncManagerGlobal *FuncManager

// FuncManager manager fc function
// create function and http trigger
// update instance env
type FuncManager struct {
	endpoints map[string][]string
	//modelToInfo map[string][]*SdModels
	funcStore          datastore.Datastore
	fcClient           *fc.Client
	fc3Client          *fc3.Client
	lock               sync.RWMutex
	lastInvokeEndpoint string
}

func isFc3() bool {
	return config.ConfigGlobal.ServiceName == ""
}

func InitFuncManager(funcStore datastore.Datastore) error {
	// init fc client
	fcEndpoint := fmt.Sprintf("%s.%s.fc.aliyuncs.com", config.ConfigGlobal.AccountId,
		config.ConfigGlobal.Region)
	FuncManagerGlobal = &FuncManager{
		endpoints: make(map[string][]string),
		funcStore: funcStore,
	}
	var err error
	if isFc3() {
		FuncManagerGlobal.fc3Client, err = fc3.NewClient(new(openapi.Config).SetAccessKeyId(config.ConfigGlobal.AccessKeyId).
			SetAccessKeySecret(config.ConfigGlobal.AccessKeySecret).SetSecurityToken(config.ConfigGlobal.AccessKeyToken).
			SetProtocol("HTTP").SetEndpoint(fcEndpoint))
	} else {
		FuncManagerGlobal.fcClient, err = fc.NewClient(new(openapi.Config).SetAccessKeyId(config.ConfigGlobal.AccessKeyId).
			SetAccessKeySecret(config.ConfigGlobal.AccessKeySecret).SetSecurityToken(config.ConfigGlobal.AccessKeyToken).
			SetProtocol("HTTP").SetEndpoint(fcEndpoint))
	}

	if err != nil {
		return err
	}
	// load func endpoint to cache
	FuncManagerGlobal.loadFunc()
	return nil
}

// GetLastInvokeEndpoint get last invoke endpoint
func (f *FuncManager) GetLastInvokeEndpoint(sdModel *string) string {
	f.lock.RLock()
	defer f.lock.RUnlock()
	if sdModel == nil || *sdModel == "" {
		return f.lastInvokeEndpoint
	} else if endpoint := f.getEndpointFromCache(*sdModel); endpoint != "" {
		f.lastInvokeEndpoint = endpoint
		return endpoint
	}
	return f.lastInvokeEndpoint
}

// GetEndpoint get endpoint, key=sdModel
// retry and read from db if create function fail
// first get from cache
// second get from db
// third create function and return endpoint
func (f *FuncManager) GetEndpoint(sdModel string) (string, error) {
	//return "http://localhost:8010", nil
	key := "default"
	if config.ConfigGlobal.GetFlexMode() == config.MultiFunc && sdModel != "" {
		key = sdModel
	}
	// retry
	reTry := 2
	for reTry > 0 {
		// first get cache
		if endpoint := f.getEndpointFromCache(key); endpoint != "" {
			f.lastInvokeEndpoint = endpoint
			return endpoint, nil
		}

		f.lock.Lock()
		// second get from db
		if endpoint := f.getEndpointFromDb(key); endpoint != "" {
			f.lastInvokeEndpoint = endpoint
			f.lock.Unlock()
			return endpoint, nil
		}
		// third create function
		if endpoint := f.createFunc(key, sdModel, getEnv(sdModel)); endpoint != "" {
			f.lastInvokeEndpoint = endpoint
			f.lock.Unlock()
			return endpoint, nil
		}
		f.lock.Unlock()
		reTry--
		time.Sleep(RETRY_INTERVALMS)
	}
	return "", errors.New("not get sd endpoint")
}

// UpdateAllFunctionEnv update instance env, restart agent function
func (f *FuncManager) UpdateAllFunctionEnv() error {
	// reload from db
	f.lock.Lock()
	f.loadFunc()
	f.lock.Unlock()
	// update all function env
	for key, _ := range f.endpoints {
		if err := f.UpdateFunctionEnv(key); err != nil {
			return err
		}
	}
	return nil
}

// UpdateFunctionEnv update instance env
// input modelName and env
func (f *FuncManager) UpdateFunctionEnv(key string) error {
	functionName := GetFunctionName(key)
	res := f.GetFuncResource(functionName)
	if res == nil {
		return nil
	}
	res.Env[config.MODEL_REFRESH_SIGNAL] = utils.String(fmt.Sprintf("%d", utils.TimestampS())) // value = now timestamp
	//compatible fc3.0
	if isFc3() {
		if _, err := f.fc3Client.UpdateFunction(&functionName,
			new(fc3.UpdateFunctionRequest).SetRequest(new(fc3.UpdateFunctionInput).SetRuntime("custom-container").
				SetEnvironmentVariables(res.Env).SetGpuConfig(new(fc3.GPUConfig).
				SetGpuMemorySize(res.GpuMemorySize).SetGpuType(res.InstanceType)))); err != nil {
			logrus.Info(err.Error())
			return err
		}
	} else {
		if _, err := f.fcClient.UpdateFunction(&config.ConfigGlobal.ServiceName, &functionName,
			new(fc.UpdateFunctionRequest).SetRuntime("custom-container").SetGpuMemorySize(res.GpuMemorySize).
				SetEnvironmentVariables(res.Env)); err != nil {
			logrus.Info(err.Error())
			return err
		}
	}
	return nil
}

// UpdateFunctionResource update function resource
func (f *FuncManager) UpdateFunctionResource(resources map[string]*FuncResource) ([]string, []string, []string) {
	success := make([]string, 0, len(resources))
	fail := make([]string, 0, len(resources))
	errs := make([]string, 0, len(resources))
	for key, resource := range resources {
		functionName := GetFunctionName(key)
		if isFc3() {
			if _, err := f.fc3Client.UpdateFunction(&functionName,
				new(fc3.UpdateFunctionRequest).SetRequest(new(fc3.UpdateFunctionInput).SetRuntime("custom-container").
					SetMemorySize(resource.MemorySize).SetCpu(resource.CPU).SetGpuConfig(new(fc3.GPUConfig).
					SetGpuType(resource.InstanceType).SetGpuMemorySize(resource.GpuMemorySize)).
					SetTimeout(resource.Timeout).SetCustomContainerConfig(new(fc3.CustomContainerConfig).
					SetImage(resource.Image)).SetEnvironmentVariables(resource.Env))); err != nil {
				fail = append(fail, functionName)
				errs = append(errs, err.Error())

			} else {
				success = append(success, key)
			}
		} else {
			if _, err := f.fcClient.UpdateFunction(&config.ConfigGlobal.ServiceName, &functionName,
				new(fc.UpdateFunctionRequest).SetRuntime("custom-container").SetGpuMemorySize(resource.GpuMemorySize).
					SetMemorySize(resource.MemorySize).SetCpu(resource.CPU).SetInstanceType(resource.InstanceType).
					SetTimeout(resource.Timeout).SetCustomContainerConfig(new(fc.CustomContainerConfig).
					SetImage(resource.Image)).SetEnvironmentVariables(resource.Env)); err != nil {
				fail = append(fail, functionName)
				errs = append(errs, err.Error())

			} else {
				success = append(success, key)
			}
		}
	}
	return success, fail, errs
}

// get endpoint from cache
func (f *FuncManager) getEndpointFromCache(key string) string {
	f.lock.RLock()
	defer f.lock.RUnlock()
	if val, ok := f.endpoints[key]; ok {
		return val[0]
	}
	return ""
}

// get endpoint from db
func (f *FuncManager) getEndpointFromDb(key string) string {
	if data, err := f.funcStore.Get(key, []string{datastore.KModelServiceSdModel,
		datastore.KModelServiceEndPoint}); err == nil && len(data) > 0 {
		// update cache
		f.endpoints[key] = []string{data[datastore.KModelServiceEndPoint].(string),
			data[datastore.KModelServiceSdModel].(string)}
		return data[datastore.KModelServiceEndPoint].(string)
	}
	return ""
}

func (f *FuncManager) createFunc(key, sdModel string, env map[string]*string) string {
	functionName := GetFunctionName(key)
	var endpoint string
	var err error
	if isFc3() {
		endpoint, err = f.createFc3Function(functionName, env)
	} else {
		serviceName := config.ConfigGlobal.ServiceName
		endpoint, err = f.createFCFunction(serviceName, functionName, env)
	}
	if err == nil && endpoint != "" {
		// update cache
		f.endpoints[key] = []string{endpoint, sdModel}
		// put func to db
		f.putFunc(key, functionName, sdModel, endpoint)
		return endpoint
	} else {
		logrus.Info(err.Error())
	}
	return ""
}

// GetFcFuncEnv get fc function env info
func (f *FuncManager) GetFcFuncEnv(functionName string) *map[string]*string {
	if funcBody := f.GetFcFunc(functionName); funcBody != nil {
		switch funcBody.(type) {
		case *fc.GetFunctionResponse:
			return &funcBody.(*fc.GetFunctionResponse).Body.EnvironmentVariables
		case *fc3.GetFunctionResponse:
			return &funcBody.(*fc3.GetFunctionResponse).Body.EnvironmentVariables
		}
	}
	return nil
}

func (f *FuncManager) GetFuncResource(functionName string) *FuncResource {
	if funcBody := f.GetFcFunc(functionName); funcBody != nil {
		switch funcBody.(type) {
		case *fc.GetFunctionResponse:
			info := funcBody.(*fc.GetFunctionResponse)
			return &FuncResource{
				Image:         *info.Body.CustomContainerConfig.Image,
				CPU:           *info.Body.Cpu,
				MemorySize:    *info.Body.MemorySize,
				GpuMemorySize: *info.Body.GpuMemorySize,
				Timeout:       *info.Body.Timeout,
				InstanceType:  *info.Body.InstanceType,
				Env:           info.Body.EnvironmentVariables,
			}
		case *fc3.GetFunctionResponse:
			info := funcBody.(*fc3.GetFunctionResponse)
			return &FuncResource{
				Image:         *info.Body.CustomContainerConfig.Image,
				CPU:           *info.Body.Cpu,
				MemorySize:    *info.Body.MemorySize,
				GpuMemorySize: *info.Body.GpuConfig.GpuMemorySize,
				Timeout:       *info.Body.Timeout,
				InstanceType:  *info.Body.GpuConfig.GpuType,
				Env:           info.Body.EnvironmentVariables,
			}
		}
	}
	return nil
}

// GetFcFunc  get fc function info
func (f *FuncManager) GetFcFunc(functionName string) interface{} {
	if isFc3() {
		if resp, err := f.fc3Client.GetFunction(&functionName, &fc3.GetFunctionRequest{}); err == nil {
			return resp
		}
	} else {
		serviceName := config.ConfigGlobal.ServiceName
		if resp, err := f.fcClient.GetFunction(&serviceName, &functionName, &fc.GetFunctionRequest{}); err == nil {
			return resp
		}
	}
	return nil
}

// load endpoint from db
func (f *FuncManager) loadFunc() {
	// load func from db
	funcAll, _ := f.funcStore.ListAll([]string{datastore.KModelServiceKey, datastore.KModelServiceEndPoint,
		datastore.KModelServiceSdModel, datastore.KModelServerImage})
	for _, data := range funcAll {
		key := data[datastore.KModelServiceKey].(string)
		//image := data[datastore.KModelServerImage].(string)
		//if image != "" && config.ConfigGlobal.Image != "" &&
		//	image != config.ConfigGlobal.Image {
		//	// update function image
		//	if err := f.UpdateFunctionImage(key); err != nil {
		//		logrus.Info("update function image err=", err.Error())
		//	}
		//	// update db
		//	f.funcStore.Update(key, map[string]interface{}{
		//		datastore.KModelServerImage: config.ConfigGlobal.Image,
		//		datastore.KModelModifyTime:  fmt.Sprintf("%d", utils.TimestampS()),
		//	})
		//}
		endpoint := data[datastore.KModelServiceEndPoint].(string)
		// init lastInvokeEndpoint
		if f.lastInvokeEndpoint == "" {
			f.lastInvokeEndpoint = endpoint
		}
		sdModel := data[datastore.KModelServiceSdModel].(string)
		f.endpoints[key] = []string{endpoint, sdModel}
	}
}

// write func into db
func (f *FuncManager) putFunc(key, functionName, sdModel, endpoint string) {
	f.funcStore.Put(key, map[string]interface{}{
		datastore.KModelServiceKey:            key,
		datastore.KModelServiceSdModel:        sdModel,
		datastore.KModelServiceFunctionName:   functionName,
		datastore.KModelServiceEndPoint:       endpoint,
		datastore.KModelServiceCreateTime:     fmt.Sprintf("%d", utils.TimestampS()),
		datastore.KModelServiceLastModifyTime: fmt.Sprintf("%d", utils.TimestampS()),
	})
}

// ---------fc2.0----------
// create fc function
func (f *FuncManager) createFCFunction(serviceName, functionName string,
	env map[string]*string) (endpoint string, err error) {
	createRequest := getCreateFuncRequest(functionName, env)
	header := &fc.CreateFunctionHeaders{
		XFcAccountId: utils.String(config.ConfigGlobal.AccountId),
	}
	// create function
	if _, err := f.fcClient.CreateFunctionWithOptions(&serviceName, createRequest,
		header, &fcService.RuntimeOptions{}); err != nil {
		return "", err
	}
	// create http triggers
	httpTriggerRequest := getHttpTrigger()
	resp, err := f.fcClient.CreateTrigger(&serviceName, &functionName, httpTriggerRequest)
	if err != nil {
		return "", err
	}
	return *(resp.Body.UrlInternet), nil

}

// get create function request
func getCreateFuncRequest(functionName string, env map[string]*string) *fc.CreateFunctionRequest {
	return &fc.CreateFunctionRequest{
		FunctionName:         utils.String(functionName),
		CaPort:               utils.Int32(config.ConfigGlobal.CAPort),
		Cpu:                  utils.Float32(config.ConfigGlobal.CPU),
		Timeout:              utils.Int32(config.ConfigGlobal.Timeout),
		InstanceType:         utils.String(config.ConfigGlobal.InstanceType),
		Runtime:              utils.String("custom-container"),
		InstanceConcurrency:  utils.Int32(config.ConfigGlobal.InstanceConcurrency),
		MemorySize:           utils.Int32(config.ConfigGlobal.MemorySize),
		DiskSize:             utils.Int32(config.ConfigGlobal.DiskSize),
		Handler:              utils.String("index.handler"),
		GpuMemorySize:        utils.Int32(config.ConfigGlobal.GpuMemorySize),
		EnvironmentVariables: env,
		CustomContainerConfig: &fc.CustomContainerConfig{
			AccelerationType: utils.String("Default"),
			Image:            utils.String(config.ConfigGlobal.Image),
			WebServerMode:    utils.Bool(true),
		},
	}
}

// get trigger request
func getHttpTrigger() *fc.CreateTriggerRequest {
	triggerConfig := make(map[string]interface{})
	triggerConfig["authType"] = config.AUTH_TYPE
	triggerConfig["methods"] = []string{config.HTTP_GET, config.HTTP_POST, config.HTTP_PUT}
	byteConfig, _ := json.Marshal(triggerConfig)
	return &fc.CreateTriggerRequest{
		TriggerName:   utils.String(config.TRIGGER_NAME),
		TriggerType:   utils.String(config.TRIGGER_TYPE),
		TriggerConfig: utils.String(string(byteConfig)),
	}
}

// ------------end fc2.0----------

// --------------fc3.0--------------
func (f *FuncManager) createFc3Function(functionName string,
	env map[string]*string) (endpoint string, err error) {
	createRequest := f.getCreateFuncRequestFc3(functionName, env)
	if createRequest == nil {
		return "", errors.New("get createFunctionRequest error")
	}
	// create function
	if _, err := f.fc3Client.CreateFunction(createRequest); err != nil {
		return "", err
	}
	// create http triggers
	httpTriggerRequest := getHttpTriggerFc3()
	resp, err := f.fc3Client.CreateTrigger(&functionName, httpTriggerRequest)
	if err != nil {
		return "", err
	}
	return *(resp.Body.HttpTrigger.UrlInternet), nil
}

// fc3.0 get create function request
func (f *FuncManager) getCreateFuncRequestFc3(functionName string, env map[string]*string) *fc3.CreateFunctionRequest {
	// get current function
	function := f.GetFcFunc(config.ConfigGlobal.ServerName)
	if function == nil {
		return nil
	}
	curFunction := function.(*fc3.GetFunctionResponse)
	input := &fc3.CreateFunctionInput{
		FunctionName:         utils.String(functionName),
		Cpu:                  utils.Float32(config.ConfigGlobal.CPU),
		Timeout:              utils.Int32(config.ConfigGlobal.Timeout),
		Runtime:              utils.String("custom-container"),
		InstanceConcurrency:  utils.Int32(config.ConfigGlobal.InstanceConcurrency),
		MemorySize:           utils.Int32(config.ConfigGlobal.MemorySize),
		DiskSize:             utils.Int32(config.ConfigGlobal.DiskSize),
		EnvironmentVariables: env,
		Handler:              utils.String("index.handler"),
		CustomContainerConfig: &fc3.CustomContainerConfig{
			AccelerationType: utils.String("Default"),
			Image:            utils.String(config.ConfigGlobal.Image),
			Port:             utils.Int32(config.ConfigGlobal.CAPort),
		},
		GpuConfig: &fc3.GPUConfig{
			GpuMemorySize: utils.Int32(config.ConfigGlobal.GpuMemorySize),
			GpuType:       utils.String(config.ConfigGlobal.InstanceType),
		},
		Role:           curFunction.Body.Role,
		VpcConfig:      curFunction.Body.VpcConfig,
		NasConfig:      curFunction.Body.NasConfig,
		OssMountConfig: curFunction.Body.OssMountConfig,
	}
	return &fc3.CreateFunctionRequest{
		Request: input,
	}
}

// get trigger request
func getHttpTriggerFc3() *fc3.CreateTriggerRequest {
	triggerConfig := make(map[string]interface{})
	triggerConfig["authType"] = config.AUTH_TYPE
	triggerConfig["methods"] = []string{config.HTTP_GET, config.HTTP_POST, config.HTTP_PUT}
	byteConfig, _ := json.Marshal(triggerConfig)
	input := &fc3.CreateTriggerInput{
		TriggerName:   utils.String(config.TRIGGER_NAME),
		TriggerType:   utils.String(config.TRIGGER_TYPE),
		TriggerConfig: utils.String(string(byteConfig)),
	}
	return &fc3.CreateTriggerRequest{
		Request: input,
	}
}

// ----------end fc3-----------

// GetFunctionName hash key, avoid generating invalid characters
func GetFunctionName(key string) string {
	return fmt.Sprintf("sd_%s", utils.Hash(key))
}

func getEnv(sdModel string) map[string]*string {
	env := map[string]*string{
		config.SD_START_PARAMS:      utils.String(config.ConfigGlobal.ExtraArgs),
		config.MODEL_SD:             utils.String(sdModel),
		config.MODEL_REFRESH_SIGNAL: utils.String(fmt.Sprintf("%d", utils.TimestampS())), // value = now timestamp
		config.OTS_INSTANCE:         utils.String(config.ConfigGlobal.OtsInstanceName),
		config.OTS_ENDPOINT:         utils.String(config.ConfigGlobal.OtsEndpoint),
	}
	if config.ConfigGlobal.OssMode == config.REMOTE {
		env[config.OSS_ENDPOINT] = utils.String(config.ConfigGlobal.OssEndpoint)
		env[config.OSS_BUCKET] = utils.String(config.ConfigGlobal.Bucket)
	}
	return env
}
