package config

var ConfigGlobal *Config

type Config struct {
	ServiceName     string
	Image           string
	AccessKeyId     string
	AccessKeySecret string
	OtsEndpoint     string

	//db
	DbSqlite string
}

func InitConfig(fn string) {
	ConfigGlobal = &Config{}
}
