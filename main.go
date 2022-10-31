package main

import (
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/google/uuid"
	flag "github.com/spf13/pflag"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"
)

// TODO: add zap logger

const (
	StatusNo       string = "no"
	StatusClean    string = "clean"
	StatusInfected string = "infected"
)

const (
	ActionNo     string = "no"
	ActionTag    string = "tag"
	ActionDelete string = "delete"
)

var (
	configPathFlag    string
	queueWaitTimeFlag int

	awsSession *session.Session
	appConfig  *Config
)

func init() {
	flag.StringVarP(&configPathFlag, "config", "c", "", "the config file path")
	flag.IntVarP(&queueWaitTimeFlag, "wait-time", "w", 10, "how long the queue waits for messages")

	flag.Parse()
}

func main() {
	cfg, err := NewConfig(configPathFlag)
	if err != nil {
		panic(err)
	}

	appConfig = cfg

	if queueWaitTimeFlag < 0 {
		queueWaitTimeFlag = 0
	}
	if queueWaitTimeFlag > 20 {
		queueWaitTimeFlag = 20
	}

	maxSize := cfg.VolumeSize * 1073741824 / 2 // in bytes

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(cfg.Region),
	})
	if err != nil {
		panic(err)
	}
	awsSession = sess

	svc := sqs.New(sess)

	queueUrl, err := svc.GetQueueUrl(&sqs.GetQueueUrlInput{
		QueueName: aws.String(cfg.Queue),
	})
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			log.Println("Updating clamav database")
			if err = updateClamDB(); err != nil {
				log.Printf("error while trying to update clamav database: %v", err)
			}
			time.Sleep(time.Hour)
		}
	}()

	for {
		log.Println("worker: Start polling")

		queueRes, err := svc.ReceiveMessage(&sqs.ReceiveMessageInput{
			QueueUrl: queueUrl.QueueUrl,
			AttributeNames: aws.StringSlice([]string{
				"SentTimestamp",
			}),
			MaxNumberOfMessages: aws.Int64(1),
			MessageAttributeNames: aws.StringSlice([]string{
				"All",
			}),
			WaitTimeSeconds: aws.Int64(int64(queueWaitTimeFlag)),
		})
		if err != nil {
			panic(err)
		}

		if len(queueRes.Messages) > 0 {
			run(queueRes.Messages, maxSize, queueUrl.QueueUrl)
		}
	}
}

func run(messages []*sqs.Message, maxSize int, queueUrl *string) {
	numMsg := len(messages)
	log.Printf("worker: Received %v messages", numMsg)

	svc := sqs.New(awsSession)

	var wg sync.WaitGroup
	wg.Add(numMsg)
	for i := range messages {
		go func(m *sqs.Message) {
			log.Println("worker: Spawned worker goroutine")
			defer wg.Done()
			if err := handleMessage(m, maxSize); err != nil {
				log.Println(err)
			}
			if _, err := svc.DeleteMessage(&sqs.DeleteMessageInput{
				QueueUrl:      queueUrl,
				ReceiptHandle: m.ReceiptHandle,
			}); err != nil {
				log.Println(err)
			}
		}(messages[i])
	}
}

type AWSEvent struct {
	Records []struct {
		EventName string `json:"eventName"`
		S3        struct {
			Bucket struct {
				Name string `json:"name"`
				ARN  string `json:"arn"`
			} `json:"bucket"`
			Object struct {
				Key  string `json:"key"`
				Size int    `json:"size"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

func handleMessage(msg *sqs.Message, maxSize int) error {
	if msg.Body != nil {
		event := new(AWSEvent)
		if err := json.Unmarshal([]byte(*msg.Body), event); err != nil {
			return err
		}
		if len(event.Records) > 0 {
			for _, record := range event.Records {
				if strings.HasPrefix(record.EventName, "ObjectCreated:") {
					if record.S3.Object.Size > maxSize {
						log.Printf("s3://%s/%s bigger than half of the EBS volume, skip", record.S3.Bucket.Name, record.S3.Object.Key)
						if appConfig.TagFiles {
							if err := tag(record.S3.Bucket.Name, record.S3.Object.Key, StatusNo); err != nil {
								return err
							}
						}
						if err := publishNotification(appConfig.SNSTopicArn, record.S3.Bucket.Name, record.S3.Object.Key, StatusNo, ActionNo); err != nil {
							return err
						}
					}
					fileName := path.Join(appConfig.TempStorage, uuid.New().String())
					// Log debug downloading file
					if err := downloadObject(record.S3.Bucket.Name, record.S3.Object.Key, fileName); err != nil {
						if aerr, ok := err.(awserr.Error); ok {
							switch aerr.Code() {
							case s3.ErrCodeNoSuchKey:
								log.Printf("s3://%s/%s does no longer exist, skip", record.S3.Bucket.Name, record.S3.Object.Key)
							}
						}
						return nil
					}
					log.Printf("scanning s3://%s/%s ...", record.S3.Bucket.Name, record.S3.Object.Key)
					exitStatus, err := runClamscan(fileName)
					if err != nil {
						log.Printf("error occured while trying to run clamscan: %v", err)
						return err
					}

					if exitStatus == 0 {
						if appConfig.TagFiles {
							log.Printf("s3://%s/%s is clean(tagging)", record.S3.Bucket.Name, record.S3.Object.Key)
							if err = tag(record.S3.Bucket.Name, record.S3.Object.Key, StatusClean); err != nil {
								return err
							}
						} else {
							log.Printf("s3://%s/%s is clean", record.S3.Bucket.Name, record.S3.Object.Key)
						}
						if appConfig.ReportCleanFiles {
							if err = publishNotification(appConfig.SNSTopicArn, record.S3.Bucket.Name, record.S3.Object.Key, StatusClean, ActionNo); err != nil {
								return err
							}
						}
					} else if exitStatus == 1 {
						if appConfig.DeleteInfectedFiles {
							log.Printf("s3://%s/%s is infected(tagging)", record.S3.Bucket.Name, record.S3.Object.Key)
							if err = deleteObject(record.S3.Bucket.Name, record.S3.Object.Key); err != nil {
								return err
							}
							if err = publishNotification(appConfig.SNSTopicArn, record.S3.Bucket.Name, record.S3.Object.Key, StatusInfected, ActionDelete); err != nil {
								return err
							}
						} else {
							if appConfig.TagFiles {
								log.Printf("s3://%s/%s is infected(tagging)", record.S3.Bucket.Name, record.S3.Object.Key)
								if err = tag(record.S3.Bucket.Name, record.S3.Object.Key, StatusInfected); err != nil {
									return err
								}
								if err = publishNotification(appConfig.SNSTopicArn, record.S3.Bucket.Name, record.S3.Object.Key, StatusInfected, ActionTag); err != nil {
									return err
								}
							} else {
								if err = publishNotification(appConfig.SNSTopicArn, record.S3.Bucket.Name, record.S3.Object.Key, StatusInfected, ActionNo); err != nil {
									return err
								}
							}
						}
					} else {
						log.Printf("s3://%s/%s could not be scanned, clamdscan exit status was %v, retry", record.S3.Bucket.Name, record.S3.Object.Key, exitStatus)
					}
					if err = os.Remove(fileName); err != nil {
						return err
					}
				}
			}
		}

	}
	return nil
}

func tag(bucket, key, status string) error {
	svc := s3.New(awsSession)

	if _, err := svc.PutObjectTagging(&s3.PutObjectTaggingInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Tagging: &s3.Tagging{
			TagSet: []*s3.Tag{
				{
					Key:   aws.String(appConfig.TagKey),
					Value: aws.String(status),
				},
			},
		},
	}); err != nil {
		return err
	}

	return nil
}

func downloadObject(bucket, key, fileName string) error {
	downloader := s3manager.NewDownloader(awsSession)

	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()

	numBytes, err := downloader.Download(f, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}

	log.Printf("downloaded: %s, bytes: %v", f.Name(), numBytes)

	return nil
}

func deleteObject(bucket, key string) error {
	svc := s3.New(awsSession)

	if _, err := svc.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		return err
	}

	return nil
}

func publishNotification(topicArn, bucket, key, status, action string) error {
	svc := sns.New(awsSession)

	if _, err := svc.Publish(&sns.PublishInput{
		TopicArn: aws.String(topicArn),
		Message:  aws.String(fmt.Sprintf("s3://%s/%s is %s, %s action executed", bucket, key, status, action)),
		Subject:  aws.String(fmt.Sprintf("s3-virus-scan s3://%s", bucket)),
	}); err != nil {
		return err
	}

	return nil
}

func runClamscan(fileName string) (int, error) {
	cmd := exec.Command("clamscan", fileName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}

	if err := cmd.Wait(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			return exiterr.ExitCode(), nil
		} else {
			return 0, err
		}
	}

	return 0, nil
}

func updateClamDB() error {
	cmd := exec.Command("freshclam")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	return nil
}