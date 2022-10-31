package main

import (
	"github.com/creasty/defaults"
	"gopkg.in/yaml.v3"
	"os"
)

type Config struct {
	// Region The AWS region
	Region string `yaml:"region" default:"eu-central-1"`
	// VolumeSize The size of the volume, in gibibytes (GiBs). You can only scan files that are smaller than half of this value.
	VolumeSize int `yaml:"volumeSize" default:"16"`
	// Queue The url of the SQS queue
	Queue string `yaml:"queue"`
	// DeleteInfectedFiles automatically delete infected files
	DeleteInfectedFiles bool `yaml:"deleteInfectedFiles" default:"true"`
	// ReportCleanFiles Report successful scan of clean, as well as infected files to the SNS topic
	ReportCleanFiles bool `yaml:"reportCleanFiles" default:"false"`
	// TagFiles Tag S3 object upon successful scan accordingly with values of "clean" or "infected" (infected only works if delete_infected_files != true) using key specified by tag_key.
	TagFiles bool `yaml:"tagFiles" default:"true"`
	// TagKey S3 object tag key used to specify values of "clean" or "infected".
	TagKey string `yaml:"tagKey" default:"clamav-status"`
	// SNSTopicArn to report to
	SNSTopicArn string `yaml:"snsTopicArn"`
	// TempStorage to store the files
	TempStorage string `yaml:"tempStorage"`
}

func NewConfig(path string) (*Config, error) {
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := new(Config)
	if err = defaults.Set(cfg); err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(f, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
