package ste

import (
	"fmt"
	"encoding/json"
	"net/http"
	"io/ioutil"
	"errors"
	"context"
	"github.com/Azure/azure-storage-azcopy/common"
	"runtime"
	"math"
	"time"
	"sync/atomic"
)
var steContext = context.Background()
var realTimeThroughputCounter = throughputState {lastCheckedBytes:0, currentBytes:0, lastCheckedTime:time.Now()}

// putJobPartInfoHandlerIntoMap api put the JobPartPlanInfo pointer for given jobId and part number in map[common.JobID]map[common.PartNumber]*JobPartPlanInfo
func putJobPartInfoHandlerIntoMap(jobHandler *JobPartPlanInfo, jobId common.JobID, partNo common.PartNumber,
									jPartInfoMap *JobPartPlanInfoMap){
	jPartInfoMap.StoreJobPartPlanInfo(jobId, partNo, jobHandler)
}

// getJobPartMapFromJobPartInfoMap api gets the map[common.PartNumber]*JobPartPlanInfo for given jobId and part number from map[common.JobID]map[common.PartNumber]*JobPartPlanInfo
func getJobPartMapFromJobPartInfoMap(jobId common.JobID,
										jPartInfoMap *JobPartPlanInfoMap)  (jPartMap map[common.PartNumber]*JobPartPlanInfo){
	jPartMap, ok := jPartInfoMap.LoadPartPlanMapforJob(jobId)
	if !ok{
		errorMsg := fmt.Sprintf("no part number exists for given jobId %s", jobId)
		panic(errors.New(errorMsg))
	}
	return jPartMap
}

// getJobPartInfoHandlerFromMap
func getJobPartInfoHandlerFromMap(jobId common.JobID, partNo common.PartNumber,
										jPartInfoMap *JobPartPlanInfoMap) (*JobPartPlanInfo, error){
	jHandler := jPartInfoMap.LoadJobPartPlanInfoForJobPart(jobId, partNo)
	if jHandler == nil{
		errorMsg := fmt.Sprintf("no jobPartPlanInfo handler exists for JobId %s and part number %d", jobId, partNo)
		return nil, errors.New(errorMsg)
	}
	return jHandler, nil
}

// ExecuteNewCopyJobPartOrder api executes a new job part order
func ExecuteNewCopyJobPartOrder(payload common.CopyJobPartOrder, coordiatorChannels *CoordinatorChannels, jPartPlanInfoMap *JobPartPlanInfoMap, jobToLoggerMap *JobToLoggerMap){
	/*
		* Convert the blobdata to memory map compatible DestinationBlobData
		* Create a file for JobPartOrder and write data into that file.
		* Initialize the JobPartOrder
		*  Create a JobPartInfo pointer for the new job and put it into the map
		* Schedule the transfers of Job by putting them into Transfermsg channels.
	 */
	 //fmt.Println("New Job Part Order Request Received", payload.ID)
	data := payload.OptionalAttributes
	var crc [128/ 8]byte
	copy(crc[:], CRC64BitExample)
	destBlobData, err := dataToDestinationBlobData(data)
	if err != nil {
		panic(err)
	}
	fileName := createJobPartPlanFile(payload, destBlobData)
	var jobHandler  = new(JobPartPlanInfo)
	jobHandler.ctx, jobHandler.cancel = context.WithCancel(context.Background())
	err = (jobHandler).initialize(jobHandler.ctx, fileName)
	if err != nil {
		panic(err)
	}
	logger := getLoggerForJobId(payload.ID, jobToLoggerMap)
	if logger == nil{
		logger = new(common.Logger)
		logger.Initialize(payload.LogVerbosity, payload.ID)
		jobToLoggerMap.StoreLoggerForJob(payload.ID, logger)
	}
	jobHandler.Logger = logger
	jobHandler.Logger.Info("new job part order received with job Id %s and part number %d", payload.ID, payload.PartNum)
	putJobPartInfoHandlerIntoMap(jobHandler, payload.ID, payload.PartNum, jPartPlanInfoMap)

	if coordiatorChannels == nil{
		jobHandler.Logger.Error("coordinator channels not initialized properly")
	}
	numTransfer := jobHandler.getJobPartPlanPointer().NumTransfers
	for index := uint32(0); index < numTransfer; index ++{
		transferMsg := TransferMsg{payload.ID, payload.PartNum, index, jPartPlanInfoMap}
		switch payload.Priority{
		case HighJobPriority:
			coordiatorChannels.HighTransfer <- transferMsg
			jobHandler.Logger.Debug("successfully scheduled transfer %v with priority %v for Job %v and part number %v", index, payload.Priority, string(payload.ID), payload.PartNum)
		case MediumJobPriority:
			coordiatorChannels.MedTransfer <- transferMsg
		case LowJobPriority:
			coordiatorChannels.LowTransfer <- transferMsg
		default:
			jobHandler.Logger.Debug("invalid job part order priority %d for given Job Id %s and part number %d and transfer Index %d", payload.Priority, payload.ID, payload.PartNum, index)
			fmt.Println()
		}
	}

	//// Test Cases
	//jobHandler.updateTheChunkInfo(0,0, crc, ChunkTransferStatusComplete)
	//
	//jobHandler.updateTheChunkInfo(1,0, crc, ChunkTransferStatusComplete)
	//
	//jobHandler.getChunkInfo(1,0)
	//
	//cInfo := jobHandler.getChunkInfo(1,2)
	//fmt.Println("Chunk Crc ", string(cInfo.BlockId[:]), " ",cInfo.Status)
	//
	//cInfo  = jobHandler.getChunkInfo(0,1)
	//fmt.Println("Chunk Crc ", string(cInfo.BlockId[:]), " ",cInfo.Status)
	//
	//cInfo  = jobHandler.getChunkInfo(0,2)
	//fmt.Println("Chunk Crc ", string(cInfo.BlockId[:]), " ",cInfo.Status)
}

func validateAndRouteHttpPostRequest(payload common.CopyJobPartOrder, coordintorChannels *CoordinatorChannels, jPartPlanInfoMap *JobPartPlanInfoMap, jobToLoggerMap *JobToLoggerMap) (bool){
	switch {
	case payload.SourceType == common.Local &&
		payload.DestinationType == common.Blob:
			ExecuteNewCopyJobPartOrder(payload, coordintorChannels, jPartPlanInfoMap, jobToLoggerMap)
			return true
	case payload.SourceType == common.Blob &&
		payload.DestinationType == common.Local:
		ExecuteNewCopyJobPartOrder(payload, coordintorChannels, jPartPlanInfoMap, jobToLoggerMap)
		return true
	default:
		fmt.Println("Command %d Type Not Supported by STE", payload)
		return false
	}
	return false
}

// getJobSummary api returns the job progress summary of an active job
/*
	* Return following Properties in Job Progress Summary
	* CompleteJobOrdered - determines whether final part of job has been ordered or not
	* TotalNumberOfTransfer - total number of transfers available for the given job
	* TotalNumberofTransferCompleted - total number of transfers in the job completed
	* NumberOfTransferCompletedafterCheckpoint - number of transfers completed after the last checkpoint
	* NumberOfTransferFailedAfterCheckpoint - number of transfers failed after last checkpoint timestamp
	* PercentageProgress - job progress reported in terms of percentage
	* FailedTransfers - list of transfer after last checkpoint timestamp that failed.
 */
func getJobSummary(jobId common.JobID, jPartPlanInfoMap *JobPartPlanInfoMap, resp *http.ResponseWriter){

	//fmt.Println("received a get job order status request for JobId ", jobId)
	// getJobPartMapFromJobPartInfoMap gives the map of partNo to JobPartPlanInfo Pointer for a given JobId
	jPartMap := getJobPartMapFromJobPartInfoMap(jobId, jPartPlanInfoMap)

	// if partNumber to JobPartPlanInfo Pointer map is nil, then returning error
	if jPartMap == nil{
		(*resp).WriteHeader(http.StatusBadRequest)
		errorMsg := fmt.Sprintf("no active job with JobId %s exists", jobId)
		(*resp).Write([]byte(errorMsg))
		return
	}

	// completeJobOrdered determines whether final part for job with JobId has been ordered or not.
	var completeJobOrdered bool = false
	// failedTransfers represents the list of transfers which failed after the last checkpoint timestamp
	var failedTransfers []common.TransferStatus

	progressSummary := common.JobProgressSummary{}
	for _, jHandler := range jPartMap{
		//fmt.Println("part no ", partNo)

		// currentJobPartPlanInfo represents the memory map JobPartPlanHeader for current partNo
		currentJobPartPlanInfo := jHandler.getJobPartPlanPointer()

		completeJobOrdered = completeJobOrdered || currentJobPartPlanInfo.IsFinalPart
		progressSummary.TotalNumberOfTransfer += currentJobPartPlanInfo.NumTransfers
		// iterating through all transfers for current partNo and job with given jobId
		for index := uint32(0); index < currentJobPartPlanInfo.NumTransfers; index++{

			// transferHeader represents the memory map transfer header of transfer at index position for given job and part number
			transferHeader := jHandler.Transfer(index)
			// check for all completed transfer to calculate the progress percentage at the end
			if transferHeader.Status == common.TransferStatusComplete{
				progressSummary.TotalNumberofTransferCompleted += 1
			}
			if transferHeader.Status == common.TransferStatusFailed{
				progressSummary.TotalNumberofFailedTransfer += 1
				// getting the source and destination for failed transfer at position - index
				source, destination := jHandler.getTransferSrcDstDetail(index)
				// appending to list of failed transfer
				failedTransfers = append(failedTransfers, common.TransferStatus{source, destination, common.TransferStatusFailed})
			}
		}
	}
	 /*If each transfer in all parts of a job has either completed or failed and is not in active or inactive state, then job order is said to be completed
	 if final part of job has been ordered.*/
	if (progressSummary.TotalNumberOfTransfer == progressSummary.TotalNumberofFailedTransfer + progressSummary.TotalNumberofTransferCompleted) &&(
		completeJobOrdered){
			progressSummary.JobStatus = common.StatusCompleted
	}
	progressSummary.CompleteJobOrdered = completeJobOrdered
	progressSummary.FailedTransfers = failedTransfers
	progressSummary.PercentageProgress = (( progressSummary.TotalNumberofTransferCompleted  + progressSummary.TotalNumberofFailedTransfer) *100)/ progressSummary.TotalNumberOfTransfer

	// get the throughput counts
	numOfBytesTransferredSinceLastCheckpoint := atomic.LoadInt64(&realTimeThroughputCounter.currentBytes) - realTimeThroughputCounter.lastCheckedBytes
	if numOfBytesTransferredSinceLastCheckpoint == 0 {
		progressSummary.ThroughputInBytesPerSeconds = 0
	} else {
		progressSummary.ThroughputInBytesPerSeconds = float64(numOfBytesTransferredSinceLastCheckpoint) / time.Since(realTimeThroughputCounter.lastCheckedTime).Seconds()
	}
	// update the throughput state
	snapshotThroughputCounter()

	// marshalling the JobProgressSummary struct to send back in response.
	jobProgressSummaryJson, err := json.MarshalIndent(progressSummary, "", "")
	if err != nil{
		result := fmt.Sprintf("error marshalling the progress summary for Job Id %s", jobId)
		(*resp).WriteHeader(http.StatusInternalServerError)
		(*resp).Write([]byte(result))
		return
	}
	(*resp).WriteHeader(http.StatusAccepted)
	(*resp).Write(jobProgressSummaryJson)
}

func updateThroughputCounter(numBytes int64) {
	atomic.AddInt64(&realTimeThroughputCounter.currentBytes, numBytes)
}

func snapshotThroughputCounter() {
	realTimeThroughputCounter.lastCheckedBytes = atomic.LoadInt64(&realTimeThroughputCounter.currentBytes)
	realTimeThroughputCounter.lastCheckedTime = time.Now()
}

func getTransferList(jobId common.JobID, expectedStatus common.Status, jPartPlanInfoMap *JobPartPlanInfoMap, resp *http.ResponseWriter) {
	// getJobPartInfoHandlerFromMap gives the JobPartPlanInfo Pointer for given JobId and PartNumber
	jPartMap, ok := jPartPlanInfoMap.LoadPartPlanMapforJob(jobId)
	// sending back the error status and error message in response
	if !ok{
		(*resp).WriteHeader(http.StatusBadRequest)
		(*resp).Write([]byte(fmt.Sprintf("invalid jobId %s", jobId)))
		return
	}
	var transferList []common.TransferStatus
	for _, jHandler := range jPartMap{
		// jPartPlan represents the memory map JobPartPlanHeader for given jobid and part number
		jPartPlan := jHandler.getJobPartPlanPointer()
		numTransfer := jPartPlan.NumTransfers

		// trasnferStatusList represents the list containing number of transfer for given jobid and part number
		for index := uint32(0); index < numTransfer; index ++{
			// getting transfer header of transfer at index index for given jobId and part number
			transferEntry := jHandler.Transfer(index)
			// if the expected status is not to list all transfer and status of current transfer is not equal to the expected status, then we skip this transfer
			if expectedStatus != common.TranferStatusAll && transferEntry.Status != expectedStatus{
				continue
			}
			// getting source and destination of a transfer at index index for given jobId and part number.
			source, destination := jHandler.getTransferSrcDstDetail(index)
			transferList = append(transferList, common.TransferStatus{source, destination, transferEntry.Status})
		}
	}
	// marshalling the TransfersStatus Struct to send back in response to front-end
	tStatusJson, err := json.MarshalIndent(common.TransfersStatus{transferList}, "", "")
	if err != nil{
		result := fmt.Sprintf("error marshalling the transfer status for Job Id %s", jobId)
		(*resp).WriteHeader(http.StatusInternalServerError)
		(*resp).Write([]byte(result))
		return
	}
	(*resp).WriteHeader(http.StatusAccepted)
	(*resp).Write(tStatusJson)
}

func listExistingJobs(jPartPlanInfoMap *JobPartPlanInfoMap, resp *http.ResponseWriter){
	jobIds := jPartPlanInfoMap.LoadExistingJobIds()

	existingJobDetails := common.ExistingJobDetails{jobIds}
	existingJobDetailsJson, err:= json.Marshal(existingJobDetails)
	if err != nil{
		(*resp).WriteHeader(http.StatusInternalServerError)
		(*resp).Write([]byte("error marshalling the existing job list"))
		return
	}
	(*resp).WriteHeader(http.StatusAccepted)
	(*resp).Write(existingJobDetailsJson)
}

func getJobOrderDetails(jobId common.JobID, jPartPlanInfoMap *JobPartPlanInfoMap, resp *http.ResponseWriter){
	// getJobPartMapFromJobPartInfoMap gives the map of partNo to JobPartPlanInfo Pointer for a given JobId
	jPartMap := getJobPartMapFromJobPartInfoMap(jobId, jPartPlanInfoMap)

	// if partNumber to JobPartPlanInfo Pointer map is nil, then returning error
	if jPartMap == nil{
		(*resp).WriteHeader(http.StatusBadRequest)
		errorMsg := fmt.Sprintf("no active job with JobId %s exists", jobId)
		(*resp).Write([]byte(errorMsg))
		return
	}
	var jobPartDetails []common.JobPartDetails
	for partNo, jHandler := range jPartMap{
		jPartDetails := common.JobPartDetails{}
		jPartDetails.PartNum = partNo
		var trasnferList []common.TransferStatus
		currentJobPartPlanInfo := jHandler.getJobPartPlanPointer()
		for index := uint32(0); index < currentJobPartPlanInfo.NumTransfers; index++{
			source, destination :=	jHandler.getTransferSrcDstDetail(index)
			trasnferList = append(trasnferList, common.TransferStatus{source, destination, jHandler.Transfer(index).Status})
		}
		jPartDetails.TransferDetails = trasnferList
		jobPartDetails = append(jobPartDetails, jPartDetails)
	}
	jobPartDetailJson, err := json.MarshalIndent(common.JobOrderDetails{jobPartDetails}, "", "")
	if err != nil{
		result := fmt.Sprintf("error marshalling the job detail for Job Id %s", jobId)
		(*resp).WriteHeader(http.StatusInternalServerError)
		(*resp).Write([]byte(result))
		return
	}
	(*resp).WriteHeader(http.StatusAccepted)
	(*resp).Write(jobPartDetailJson)
}

func parsePostHttpRequest(req *http.Request) (common.CopyJobPartOrder, error){
	var payload common.CopyJobPartOrder
	if req.Body == nil{
		return payload, errors.New(InvalidHttpRequestBody)
	}
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return payload, errors.New(HttpRequestBodyReadError)
	}
	err = json.Unmarshal(body, &payload)
	if err != nil {
		return payload, errors.New(HttpRequestUnmarshalError)
	}
	return payload, nil
}

func serveRequest(resp http.ResponseWriter, req *http.Request, coordinatorChannels *CoordinatorChannels, jPartPlanInfoMap *JobPartPlanInfoMap, jobToLoggerMap *JobToLoggerMap){
	switch req.Method {
	case "GET":
		var params = req.URL.Query()["command"][0]
		listCommand := []byte(params)
		var lsCommand common.ListJobPartsTransfers
		err := json.Unmarshal(listCommand,  &lsCommand)
		if err != nil{
			panic(err)
		}
		if lsCommand.JobId == "" {
			listExistingJobs(jPartPlanInfoMap, &resp)
		}else if lsCommand.ExpectedTransferStatus == math.MaxUint8{
			getJobSummary(lsCommand.JobId, jPartPlanInfoMap, &resp)
		}else{
			getTransferList(lsCommand.JobId, lsCommand.ExpectedTransferStatus, jPartPlanInfoMap, &resp)
		}

	case "POST":
		jobRequestData, err := parsePostHttpRequest(req)
		if err != nil {
			resp.WriteHeader(http.StatusBadRequest)
			resp.Write([]byte(UnsuccessfulAZCopyRequest + " : " + err.Error()))
		}
		isValid := validateAndRouteHttpPostRequest(jobRequestData, coordinatorChannels, jPartPlanInfoMap, jobToLoggerMap)
		if isValid {
			resp.WriteHeader(http.StatusAccepted)
			resp.Write([]byte(SuccessfulAZCopyRequest))
		} else{
			resp.WriteHeader(http.StatusBadRequest)
			resp.Write([]byte(UnsuccessfulAZCopyRequest))
		}
	case "PUT":

	case "DELETE":

	default:
		fmt.Println("Operation Not Supported by STE")
		resp.WriteHeader(http.StatusBadRequest)
		resp.Write([]byte(UnsuccessfulAZCopyRequest))
	}
}

// InitializedChannels initializes the channels used further by coordinator and execution engine
func InitializedChannels() (*CoordinatorChannels, *EEChannels){

	// HighTransferMsgChannel takes high priority job part transfers from coordinator and feed to execution engine
	HighTransferMsgChannel := make(chan TransferMsg, 500)
	// MedTransferMsgChannel takes high priority job part transfers from coordinator and feed to execution engine
	MedTransferMsgChannel := make(chan TransferMsg, 500)
	// LowTransferMsgChannel takes high priority job part transfers from coordinator and feed to execution engine
	LowTransferMsgChannel := make(chan TransferMsg, 500)

	// HighChunkMsgChannel queues high priority job part transfer chunk transactions
	HighChunkMsgChannel := make(chan ChunkMsg, 500)
	// MedChunkMsgChannel queues medium priority job part transfer chunk transactions
	MedChunkMsgChannel := make(chan ChunkMsg, 500)
	// LowChunkMsgChannel queues low priority job part transfer chunk transactions
	LowChunkMsgChannel := make(chan ChunkMsg, 500)

	// Create suicide channel which is used to scale back on the number of workers
	SuicideChannel := make(chan SuicideJob, 100)

	transferEngineChannel := &CoordinatorChannels{
		HighTransfer : HighTransferMsgChannel,
		MedTransfer	: MedTransferMsgChannel,
		LowTransfer : LowTransferMsgChannel,
	}

	executionEngineChanel := &EEChannels{
		HighTransfer:HighTransferMsgChannel,
		MedTransfer:MedTransferMsgChannel,
		LowTransfer:LowTransferMsgChannel,
		HighChunkTransaction:HighChunkMsgChannel,
		MedChunkTransaction:MedChunkMsgChannel,
		LowChunkTransaction:LowChunkMsgChannel,
		SuicideChannel: SuicideChannel,
	}
	return transferEngineChannel, executionEngineChanel
}

// initializeCoordinator initializes the coordinator
/*
	* reconstructs the existing job using job part file on disk
	* creater a server listening on port 1337 for job part order requests from front end
 */
func initializeCoordinator(coordinatorChannels *CoordinatorChannels) {

	jobHandlerMap := NewJobPartPlanInfoMap()
	jobLoggerMap := NewJobToLoggerMap()
	reconstructTheExistingJobPart(jobHandlerMap)
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		serveRequest(writer, request, coordinatorChannels, jobHandlerMap, jobLoggerMap)
	})
	err := http.ListenAndServe("localhost:1337", nil)
	fmt.Print("Server Created")
	if err != nil{
		fmt.Print("Server already initialized")
	}
}

// InitializeSTE initializes the coordinator channels, execution engine channels, coordinator and execution engine
func InitializeSTE(){
	runtime.GOMAXPROCS(4)
	coordinatorChannel, execEngineChannels := InitializedChannels()
	go InitializeExecutionEngine(execEngineChannels)
	initializeCoordinator(coordinatorChannel)
}