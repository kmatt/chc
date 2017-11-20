package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	//	"github.com/davecgh/go-spew/spew"
	"github.com/satori/go.uuid" // generate sessionID and queryID
	"io"
	// "io/ioutil"
	"log"
	//"math"
	"net"
	"net/http"
	//	"net/url"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

var sessionID string = uuid.NewV4().String()

type progressInfo struct {
	Elapsed         float64
	ReadRows        uint64
	ReadBytes       uint64
	TotalRowsApprox uint64
	WrittenRows     uint64
	WrittenBytes    uint64
	MemoryUsage     int64
}

type queryStats struct {
	QueryDurationMs uint64
	ReadRows        uint64
	ReadBytes       uint64
	WrittenRows     uint64
	WrittenBytes    uint64
	ResultRows      uint64
	ResultBytes     uint64
	MemoryUsage     uint64
	Exception       string
	StackTrace      string
}

func getServerVersion() (version string, err error) {
	data, err := serviceRequest("SELECT version()")
	if err != nil {
		return
	}
	version = data[0][0]
	return
}

func getProgressInfo(queryID string) (pi progressInfo, err error) {
	pi = progressInfo{}
	query := fmt.Sprintf("select elapsed,read_rows,read_bytes,totalRows_approx,written_rows,written_bytes,memory_usage from system.processes where queryID='%s'", queryID)

	data, err := serviceRequest(query)

	if err != nil {
		return
	}
	if len(data) != 1 || len(data[0]) != 7 {
		err = errors.New("Bad response dimensions")
		return
	}

	pi.Elapsed, _ = strconv.ParseFloat(data[0][0], 64)
	pi.ReadRows, _ = strconv.ParseUint(data[0][1], 10, 64)
	pi.ReadBytes, _ = strconv.ParseUint(data[0][2], 10, 64)
	pi.TotalRowsApprox, _ = strconv.ParseUint(data[0][3], 10, 64)
	pi.WrittenRows, _ = strconv.ParseUint(data[0][4], 10, 64)
	pi.WrittenBytes, _ = strconv.ParseUint(data[0][5], 10, 64)
	pi.MemoryUsage, _ = strconv.ParseInt(data[0][6], 10, 64)
	//spew.Dump(pi)
	return
}

func getQueryStats(queryID string) (qs queryStats, err error) {

	query := fmt.Sprintf("select query_duration_ms,read_rows,read_bytes,written_rows,written_bytes,result_rows,result_bytes,memory_usage,exception,stack_trace,type from system.query_log where queryID='%s' and type>1", queryID)

	data, err := serviceRequest(query)

	if err != nil {
		return
	}
	if len(data) != 1 || len(data[0]) != 7 {
		err = errors.New("Bad response dimensions")
		return
	}

	qs.QueryDurationMs, _ = strconv.ParseUint(data[0][0], 10, 64)
	qs.ReadRows, _ = strconv.ParseUint(data[0][1], 10, 64)
	qs.ReadBytes, _ = strconv.ParseUint(data[0][2], 10, 64)
	qs.WrittenRows, _ = strconv.ParseUint(data[0][3], 10, 64)
	qs.WrittenBytes, _ = strconv.ParseUint(data[0][4], 10, 64)
	qs.ResultRows, _ = strconv.ParseUint(data[0][5], 10, 64)
	qs.ResultBytes, _ = strconv.ParseUint(data[0][6], 10, 64)
	qs.MemoryUsage, _ = strconv.ParseUint(data[0][7], 10, 64)
	qs.Exception = data[0][8]
	qs.StackTrace = data[0][9]
	return
}

func queryToStdout(cx context.Context, query string, stdOut, stdErr io.Writer) int {

	queryID := uuid.NewV4().String()
	status := -1

	errorChannel := make(chan error)
	dataChannel := make(chan string)
	doneChannel := make(chan bool)
	statusCodeChannel := make(chan int)
	progressChannel := make(chan progressInfo)

	initProgress()

	start := time.Now()
	finishTickerChannel := make(chan bool, 3)

	go func() {

		ticker := time.NewTicker(time.Millisecond * 125)

	Loop3:
		for {
			select {
			case <-ticker.C:
				pi, err := getProgressInfo(queryID)
				if err == nil {
					progressChannel <- pi
				}
			case <-finishTickerChannel:
				break Loop3
			}
		}
		ticker.Stop()

		// println("ticker funct finished")
	}()

	go func() {
		extraSettings := map[string]string{"log_queries": "1", "queryID": queryID, "sessionID": sessionID}
		req := prepareRequest(query, opts.Format, extraSettings).WithContext(cx)

		response, err := http.DefaultClient.Do(req)
		select {
		case <-cx.Done():
			// Already timedout
		default:
			if err != nil {
				errorChannel <- err
			} else {
				defer response.Body.Close()

				statusCodeChannel <- response.StatusCode
				reader := bufio.NewReader(response.Body)
			Loop:
				for {
					//						io.WriteString(stdErr, "Debug P__\n\n" );
					select {
					case <-cx.Done():
						break Loop
					default:
						msg, err := reader.ReadString('\n')
						//spew.Dump(err)
						//spew.Dump(msg)
						if err == io.EOF {
							doneChannel <- true
							break Loop
						} else if err == nil {
							dataChannel <- msg
						} else {
							errorChannel <- err
							break Loop
						}
					}
				}
			}

		}
		//     println("do funct finished")
	}()
Loop2:
	for {
		select {
		case st := <-statusCodeChannel:
			status = st
		case <-cx.Done():
			finishTickerChannel <- true // aware deadlocks here, we uses buffered channel here
			clearProgress(stdErr)
			io.WriteString(stdErr, fmt.Sprintf("\nKilling query (id: %v)... ", queryID))
			if killQuery(queryID) {
				io.WriteString(stdErr, "killed!\n\n")
			} else {
				io.WriteString(stdErr, "failure!\n\n")
			}
			break Loop2
		case err := <-errorChannel:
			log.Fatalln(err)
		case pi := <-progressChannel:
			writeProgres(stdErr, pi.ReadRows, pi.ReadBytes, pi.TotalRowsApprox, uint64(pi.Elapsed*1000000000))
		case <-doneChannel:
			finishTickerChannel <- true // aware deadlocks here, we uses buffered channel here
			clearProgress(stdErr)
			io.WriteString(stdErr, fmt.Sprintf("\nElapsed: %v\n\n", time.Since(start)))
			break Loop2
		case data := <-dataChannel:
			clearProgress(stdErr)
			io.WriteString(stdOut, data)
		}
	}
	return status
	// io.WriteString(stdErr, "queryToStdout finished" );
}

func queryToStdout2(query string, stdOut, stdErr io.Writer) {
	stdOutBuffered := bufio.NewWriter(stdOut)
	stdErrBuffered := bufio.NewWriter(stdErr)

	extraSettings := map[string]string{"send_progress_in_http_headers": "1"}

	req := prepareRequest(query, opts.Format, extraSettings)

	initProgress()
	start := time.Now()

	// connect to this socket
	conn, err := net.Dial("tcp", getHost())
	if err != nil {
		log.Fatalln(err) // TODO - process that / retry?
	}

	err = req.Write(conn)
	if err != nil {
		log.Fatalln(err) // TODO - process that / retry?
	}

	var requestBeginning bytes.Buffer
	tee := io.TeeReader(conn, &requestBeginning)
	reader := bufio.NewReader(tee)
	for {
		msg, err := reader.ReadString('\n')
		if err == io.EOF {
			break
			// Ups... We have EOF before HTTP headers finished...
			// TODO - process that / retry?
		}
		if err != nil {
			log.Fatalln(err) // TODO - process that / retry?
		}
		message := strings.TrimSpace(msg)
		if message == "" {
			break // header finished
		}

		//	fmt.Print(message)
		if strings.HasPrefix(message, "X-ClickHouse-Progress:") {
			type ProgressData struct {
				ReadRows  uint64 `json:"read_rows,string"`
				ReadBytes uint64 `json:"read_bytes,string"`
				TotalRows uint64 `json:"total_rows,string"`
			}

			progressDataJSON := strings.TrimSpace(message[22:])
			var pd ProgressData
			err := json.Unmarshal([]byte(progressDataJSON), &pd)
			if err != nil {
				log.Fatal(err)
			}

			writeProgres(stdErrBuffered, pd.ReadRows, pd.ReadBytes, pd.TotalRows, uint64(time.Since(start)))
			stdErrBuffered.Flush()
		}
	}

	reader2 := io.MultiReader(&requestBeginning, conn)
	reader3 := bufio.NewReader(reader2)
	res, err := http.ReadResponse(reader3, req)
	if err != nil {
		log.Fatal(err)
	}

	clearProgress(stdErrBuffered)
	stdErrBuffered.Flush()

	//fmt.Println(res.StatusCode)
	//fmt.Println(res.ContentLength)
	defer res.Body.Close()
	_, err = io.Copy(stdOutBuffered, res.Body)
	if err != nil {
		log.Fatal(err)
	}
	stdOutBuffered.Flush()
	//  fmt.Println(res.Body.Read())
}
