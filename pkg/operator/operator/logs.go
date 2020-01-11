/*
Copyright 2019 Cortex Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workloads

import (
	"encoding/json"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/gorilla/websocket"
	"gopkg.in/karalabe/cookiejar.v2/collections/deque"

	awslib "github.com/cortexlabs/cortex/pkg/lib/aws"
	"github.com/cortexlabs/cortex/pkg/lib/sets/strset"
	s "github.com/cortexlabs/cortex/pkg/lib/strings"
	libtime "github.com/cortexlabs/cortex/pkg/lib/time"
	"github.com/cortexlabs/cortex/pkg/operator/api/resource"
	"github.com/cortexlabs/cortex/pkg/operator/config"
)

const (
	_socketWriteDeadlineWait = 10 * time.Second
	_socketCloseGracePeriod  = 10 * time.Second
	_socketMaxMessageSize    = 8192
	_maxCacheSize            = 10000
	_maxLogLinesPerRequest   = 500
	_maxStreamsPerRequest    = 50
	_pollPeriod              = 250 * time.Millisecond
	_streamRefreshPeriod     = 2 * time.Second
)

type FluentdLog struct {
	Log string `json:"log"`
}

type eventCache struct {
	size       int
	seen       strset.Set
	eventQueue *deque.Deque
}

func newEventCache(cacheSize int) eventCache {
	return eventCache{
		size:       cacheSize,
		seen:       strset.New(),
		eventQueue: deque.New(),
	}
}

func (c *eventCache) Has(eventID string) bool {
	return c.seen.Has(eventID)
}

func (c *eventCache) Add(eventID string) {
	if c.eventQueue.Size() == c.size {
		eventID := c.eventQueue.PopLeft().(string)
		c.seen.Remove(eventID)
	}
	c.seen.Add(eventID)
	c.eventQueue.PushRight(eventID)
}

func ReadLogs(apiName string, socket *websocket.Conn) {
	podCheckCancel := make(chan struct{})
	defer close(podCheckCancel)
	go StreamFromCloudWatch(apiName, podCheckCancel, socket)
	pumpStdin(socket)
	podCheckCancel <- struct{}{}
}

func pumpStdin(socket *websocket.Conn) {
	socket.SetReadLimit(_socketMaxMessageSize)
	for {
		_, _, err := socket.ReadMessage()
		if err != nil {
			break
		}
	}
}

func StreamFromCloudWatch(apiName string, podCheckCancel chan struct{}, socket *websocket.Conn) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	eventCache := newEventCache(_maxCacheSize)
	lastLogTime := time.Now()
	lastLogStreamUpdateTime := time.Now().Add(-1 * _streamRefreshPeriod)
	logStreamNames := strset.New()

	// if ctx == nil {
	// 	writeAndCloseSocket(socket, "\ndeployment "+appName+" not found") // iris api not found
	// 	return
	// }

	logGroupName, err := getLogGroupName(ctx, podLabels)
	if err != nil {
		writeAndCloseSocket(socket, err.Error()) // unexpected
		return
	}

	for {
		select {
		case <-podCheckCancel:
			return
		case <-timer.C:
			ctx = CurrentContext(appName)

			if ctx == nil {
				writeAndCloseSocket(socket, "\ndeployment "+appName+" not found")
				continue
			}

			if ctx.ID != currentContextID {
				if len(currentContextID) != 0 {
					if podLabels["workloadType"] == resource.APIType.String() {
						apiName := podLabels["apiName"]
						if _, ok := ctx.APIs[apiName]; !ok {
							writeAndCloseSocket(socket, "\napi "+apiName+" was not found in latest deployment")
							continue
						}
						writeString(socket, "\na new deployment was detected, streaming logs from the latest deployment")
					} else {
						writeAndCloseSocket(socket, "\nlogging non-api workloads is not supported") // unexpected
						continue
					}
				} else {
					lastLogTime, _ = getPodStartTime(podLabels)
				}

				currentContextID = ctx.ID
				writeString(socket, "fetching logs...")
			}

			if lastLogStreamUpdateTime.Add(_streamRefreshPeriod).Before(time.Now()) {
				newLogStreamNames, err := getLogStreams(logGroupName)
				if err != nil {
					writeAndCloseSocket(socket, "error encountered while searching for log streams: "+err.Error())
					continue
				}

				if !logStreamNames.IsEqual(newLogStreamNames) {
					lastLogTime = lastLogTime.Add(-_streamRefreshPeriod)
					logStreamNames = newLogStreamNames
				}
				lastLogStreamUpdateTime = time.Now()
			}

			if len(logStreamNames) == 0 {
				timer.Reset(_pollPeriod)
				continue
			}

			endTime := libtime.ToMillis(time.Now())

			logEventsOutput, err := config.AWS.CloudWatchLogsClient.FilterLogEvents(&cloudwatchlogs.FilterLogEventsInput{
				LogGroupName:   aws.String(logGroupName),
				LogStreamNames: aws.StringSlice(logStreamNames.Slice()),
				StartTime:      aws.Int64(libtime.ToMillis(lastLogTime.Add(-_pollPeriod))),
				EndTime:        aws.Int64(endTime),
				Limit:          aws.Int64(int64(_maxLogLinesPerRequest)),
			})

			if err != nil {
				if !awslib.CheckErrCode(err, cloudwatchlogs.ErrCodeResourceNotFoundException) {
					writeAndCloseSocket(socket, "error encountered while fetching logs from cloudwatch: "+err.Error())
					continue
				}
			}

			lastLogTimestampMillis := libtime.ToMillis(lastLogTime)
			for _, logEvent := range logEventsOutput.Events {
				var log FluentdLog
				json.Unmarshal([]byte(*logEvent.Message), &log)
				if !eventCache.Has(*logEvent.EventId) {
					socket.WriteMessage(websocket.TextMessage, []byte(log.Log))
					if *logEvent.Timestamp > lastLogTimestampMillis {
						lastLogTimestampMillis = *logEvent.Timestamp
					}
					eventCache.Add(*logEvent.EventId)
				}
			}

			lastLogTime = libtime.MillisToTime(lastLogTimestampMillis)
			if len(logEventsOutput.Events) == _maxLogLinesPerRequest {
				writeString(socket, "---- Showing at most "+s.Int(_maxLogLinesPerRequest)+" lines. Visit AWS cloudwatch logs console and navigate to log group \""+logGroupName+"\" for complete logs ----")
				lastLogTime = libtime.MillisToTime(endTime)
			}

			timer.Reset(_pollPeriod)
		}
	}
}

func getLogStreams(logGroupName string) (strset.Set, error) {
	describeLogStreamsOutput, err := config.AWS.CloudWatchLogsClient.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
		OrderBy:      aws.String(cloudwatchlogs.OrderByLastEventTime),
		Descending:   aws.Bool(true),
		LogGroupName: aws.String(logGroupName),
		Limit:        aws.Int64(_maxStreamsPerRequest),
	})
	if err != nil {
		if !awslib.CheckErrCode(err, cloudwatchlogs.ErrCodeResourceNotFoundException) {
			return nil, err
		}
		return nil, nil
	}

	streams := strset.New()

	for _, stream := range describeLogStreamsOutput.LogStreams {
		streams.Add(*stream.LogStreamName)
	}
	return streams, nil
}

func getPodStartTime(searchLabels map[string]string) (time.Time, error) {
	pods, err := config.Kubernetes.ListPodsByLabels(searchLabels)
	if err != nil {
		return time.Time{}, err
	}

	if len(pods) == 0 {
		return time.Now(), nil
	}

	startTime := pods[0].CreationTimestamp.Time
	for _, pod := range pods[1:] {
		if pod.CreationTimestamp.Time.Before(startTime) {
			startTime = pod.CreationTimestamp.Time
		}
	}

	return startTime, nil
}

func getLogGroupName(apiName string) (string, error) {
	return config.LogGroup + "/" + apiName
}

func writeString(socket *websocket.Conn, message string) {
	socket.WriteMessage(websocket.TextMessage, []byte(message))
}

func closeSocket(socket *websocket.Conn) {
	socket.SetWriteDeadline(time.Now().Add(_socketWriteDeadlineWait))
	socket.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	time.Sleep(_socketCloseGracePeriod)
}

func writeAndCloseSocket(socket *websocket.Conn, message string) {
	writeString(socket, message)
	closeSocket(socket)
}
