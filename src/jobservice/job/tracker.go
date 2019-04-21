// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package job

import (
	"context"
	"encoding/json"
	"github.com/goharbor/harbor/src/jobservice/common/query"
	"github.com/goharbor/harbor/src/jobservice/common/rds"
	"github.com/goharbor/harbor/src/jobservice/common/utils"
	"github.com/goharbor/harbor/src/jobservice/errs"
	"github.com/goharbor/harbor/src/jobservice/logger"
	"github.com/gomodule/redigo/redis"
	"github.com/pkg/errors"
	"math/rand"
	"strconv"
	"time"
)

const (
	// Try best to keep the job stats data but anyway clear it after a long time
	statDataExpireTime = 180 * 24 * 3600
)

// Tracker is designed to track the life cycle of the job described by the stats
// The status change is linear and then has strict preorder and successor
// Check should be enforced before switching
//
// Pending is default status when creating job, so no need to switch
type Tracker interface {
	// Save the job stats which tracked by this tracker to the backend
	//
	// Return:
	//   none nil error returned if any issues happened
	Save() error

	// Load the job stats which tracked by this tracker with the backend data
	//
	// Return:
	//   none nil error returned if any issues happened
	Load() error

	// Get the job stats which tracked by this tracker
	//
	// Returns:
	//  *models.Info : job stats data
	Job() *Stats

	// Update the properties of the job stats
	//
	// fieldAndValues ...interface{} : One or more properties being updated
	//
	// Returns:
	//  error if update failed
	Update(fieldAndValues ...interface{}) error

	// Executions returns the executions of the job tracked by this tracker.
	// Please pay attention, this only for periodic job.
	//
	// Returns:
	//   job execution IDs matched the query
	//   the total number
	//   error if any issues happened
	Executions(q *query.Parameter) ([]string, int64, error)

	// NumericID returns the numeric ID of periodic job.
	// Please pay attention, this only for periodic job.
	NumericID() (int64, error)

	// Mark the periodic job execution to done by update the score
	// of the relation between its periodic policy and execution to -1.
	PeriodicExecutionDone() error

	// Check in message
	CheckIn(message string) error

	// Update status with retry enabled
	UpdateStatusWithRetry(targetStatus Status) error

	// The current status of job
	Status() (Status, error)

	// Expire the job stats data
	Expire() error

	// Switch status to running
	Run() error

	// Switch status to stopped
	Stop() error

	// Switch the status to error
	Fail() error

	// Switch the status to success
	Succeed() error
}

// basicTracker implements Tracker interface based on redis
type basicTracker struct {
	namespace string
	context   context.Context
	pool      *redis.Pool
	jobID     string
	jobStats  *Stats
	callback  HookCallback
}

// NewBasicTrackerWithID builds a tracker with the provided job ID
func NewBasicTrackerWithID(
	jobID string,
	ctx context.Context,
	ns string,
	pool *redis.Pool,
	callback HookCallback,
) Tracker {
	return &basicTracker{
		namespace: ns,
		context:   ctx,
		pool:      pool,
		jobID:     jobID,
		callback:  callback,
	}
}

// NewBasicTrackerWithStats builds a tracker with the provided job stats
func NewBasicTrackerWithStats(
	stats *Stats,
	ctx context.Context,
	ns string,
	pool *redis.Pool,
	callback HookCallback,
) Tracker {
	return &basicTracker{
		namespace: ns,
		context:   ctx,
		pool:      pool,
		jobStats:  stats,
		jobID:     stats.Info.JobID,
		callback:  callback,
	}
}

// Refresh the job stats which tracked by this tracker
func (bt *basicTracker) Load() error {
	return bt.retrieve()
}

// Job returns the job stats which tracked by this tracker
func (bt *basicTracker) Job() *Stats {
	return bt.jobStats
}

// Update the properties of the job stats
func (bt *basicTracker) Update(fieldAndValues ...interface{}) error {
	if len(fieldAndValues) == 0 {
		errors.New("no properties specified to update")
	}

	conn := bt.pool.Get()
	defer conn.Close()

	key := rds.KeyJobStats(bt.namespace, bt.jobID)
	args := []interface{}{"update_time", time.Now().Unix()} // update timestamp
	args = append(args, fieldAndValues...)

	return rds.HmSet(conn, key, args...)
}

// Status returns the current status of job tracked by this tracker
func (bt *basicTracker) Status() (Status, error) {
	// Retrieve the latest status again in case get the outdated one.
	conn := bt.pool.Get()
	defer conn.Close()

	rootKey := rds.KeyJobStats(bt.namespace, bt.jobID)
	return getStatus(conn, rootKey)
}

// NumericID returns the numeric ID of the periodic job
func (bt *basicTracker) NumericID() (int64, error) {
	if bt.jobStats.Info.NumericPID > 0 {
		return bt.jobStats.Info.NumericPID, nil
	}

	return -1, errors.Errorf("numeric ID not found for job: %s", bt.jobID)
}

// PeriodicExecutionDone mark the execution done
func (bt *basicTracker) PeriodicExecutionDone() error {
	if utils.IsEmptyStr(bt.jobStats.Info.UpstreamJobID) {
		return errors.Errorf("%s is not periodic job execution", bt.jobID)
	}

	key := rds.KeyUpstreamJobAndExecutions(bt.namespace, bt.jobStats.Info.UpstreamJobID)

	conn := bt.pool.Get()
	defer conn.Close()

	args := []interface{}{key, "XX", -1, bt.jobID}
	_, err := conn.Do("ZADD", args...)

	return err
}

// Check in message
func (bt *basicTracker) CheckIn(message string) error {
	if utils.IsEmptyStr(message) {
		return errors.New("check in error: empty message")
	}

	now := time.Now().Unix()
	current := Status(bt.jobStats.Info.Status)

	bt.refresh(current, message)
	err := bt.fireHookEvent(current, message)
	err = bt.Update(
		"check_in", message,
		"check_in_at", now,
		"update_time", now,
	)

	return err
}

// Executions of the tracked job
func (bt *basicTracker) Executions(q *query.Parameter) ([]string, int64, error) {
	if bt.jobStats.Info.JobKind != KindPeriodic {
		return nil, 0, errors.New("only periodic job has executions")
	}

	conn := bt.pool.Get()
	defer conn.Close()

	key := rds.KeyUpstreamJobAndExecutions(bt.namespace, bt.jobID)

	// Query executions by "non stopped"
	if nonStoppedOnly, ok := q.Extras.Get(query.ExtraParamKeyNonStoppedOnly); ok {
		if v, yes := nonStoppedOnly.(bool); yes && v {
			return queryExecutions(conn, key, q)
		}
	}

	// Pagination
	var pageNumber, pageSize uint = 1, query.DefaultPageSize
	if q != nil {
		if q.PageNumber > 0 {
			pageNumber = q.PageNumber
		}
		if q.PageSize > 0 {
			pageSize = q.PageSize
		}
	}

	// Get total first
	total, err := redis.Int64(conn.Do("ZCARD", key))
	if err != nil {
		return nil, 0, err
	}

	// No items
	result := make([]string, 0)
	if total == 0 || (int64)((pageNumber-1)*pageSize) >= total {
		return result, total, nil
	}

	min, max := (pageNumber-1)*pageSize, pageNumber*pageSize-1
	args := []interface{}{key, min, max}
	list, err := redis.Values(conn.Do("ZREVRANGE", args...))
	if err != nil {
		return nil, 0, err
	}

	for _, item := range list {
		if eID, ok := item.([]byte); ok {
			result = append(result, string(eID))
		}
	}

	return result, total, nil
}

// Expire job stats
func (bt *basicTracker) Expire() error {
	conn := bt.pool.Get()
	defer conn.Close()

	key := rds.KeyJobStats(bt.namespace, bt.jobID)
	num, err := conn.Do("EXPIRE", key, statDataExpireTime)
	if err != nil {
		return err
	}

	if num == 0 {
		return errors.Errorf("job stats for expiring %s does not exist", bt.jobID)
	}

	return nil
}

// Run job
// Either one is failed, the final return will be marked as failed.
func (bt *basicTracker) Run() error {
	err := bt.compareAndSet(RunningStatus)
	if !errs.IsStatusMismatchError(err) {
		bt.refresh(RunningStatus)
		if er := bt.fireHookEvent(RunningStatus); err == nil && er != nil {
			return er
		}
	}

	return err
}

// Stop job
// Stop is final status, if failed to do, retry should be enforced.
// Either one is failed, the final return will be marked as failed.
func (bt *basicTracker) Stop() error {
	err := bt.UpdateStatusWithRetry(StoppedStatus)
	if !errs.IsStatusMismatchError(err) {
		bt.refresh(StoppedStatus)
		if er := bt.fireHookEvent(StoppedStatus); err == nil && er != nil {
			return er
		}
	}

	return err
}

// Fail job
// Fail is final status, if failed to do, retry should be enforced.
// Either one is failed, the final return will be marked as failed.
func (bt *basicTracker) Fail() error {
	err := bt.UpdateStatusWithRetry(ErrorStatus)
	if !errs.IsStatusMismatchError(err) {
		bt.refresh(ErrorStatus)
		if er := bt.fireHookEvent(ErrorStatus); err == nil && er != nil {
			return er
		}
	}

	return err
}

// Succeed job
// Succeed is final status, if failed to do, retry should be enforced.
// Either one is failed, the final return will be marked as failed.
func (bt *basicTracker) Succeed() error {
	err := bt.UpdateStatusWithRetry(SuccessStatus)
	if !errs.IsStatusMismatchError(err) {
		bt.refresh(SuccessStatus)
		if er := bt.fireHookEvent(SuccessStatus); err == nil && er != nil {
			return er
		}
	}

	return err
}

// Save the stats of job tracked by this tracker
func (bt *basicTracker) Save() (err error) {
	if bt.jobStats == nil {
		errors.New("nil job stats to save")
	}

	conn := bt.pool.Get()
	defer conn.Close()

	// Alliance
	stats := bt.jobStats

	key := rds.KeyJobStats(bt.namespace, stats.Info.JobID)
	args := make([]interface{}, 0)
	args = append(args, key)
	args = append(args,
		"id", stats.Info.JobID,
		"name", stats.Info.JobName,
		"kind", stats.Info.JobKind,
		"unique", stats.Info.IsUnique,
		"status", stats.Info.Status,
		"ref_link", stats.Info.RefLink,
		"enqueue_time", stats.Info.EnqueueTime,
		"run_at", stats.Info.RunAt,
		"cron_spec", stats.Info.CronSpec,
		"web_hook_url", stats.Info.WebHookURL,
		"numeric_policy_id", stats.Info.NumericPID,
	)
	if stats.Info.CheckInAt > 0 && !utils.IsEmptyStr(stats.Info.CheckIn) {
		args = append(args,
			"check_in", stats.Info.CheckIn,
			"check_in_at", stats.Info.CheckInAt,
		)
	}
	if stats.Info.DieAt > 0 {
		args = append(args, "die_at", stats.Info.DieAt)
	}

	if !utils.IsEmptyStr(stats.Info.UpstreamJobID) {
		args = append(args, "upstream_job_id", stats.Info.UpstreamJobID)
	}

	if len(stats.Info.Parameters) > 0 {
		if bytes, err := json.Marshal(&stats.Info.Parameters); err == nil {
			args = append(args, "parameters", string(bytes))
		}
	}
	// Set update timestamp
	args = append(args, "update_time", time.Now().Unix())

	// Do it in a transaction
	err = conn.Send("MULTI")
	err = conn.Send("HMSET", args...)

	// If job kind is periodic job, expire time should not be set
	// If job kind is scheduled job, expire time should be runAt+
	if stats.Info.JobKind != KindPeriodic {
		var expireTime int64 = statDataExpireTime
		if stats.Info.JobKind == KindScheduled {
			nowTime := time.Now().Unix()
			future := stats.Info.RunAt - nowTime
			if future > 0 {
				expireTime += future
			}
		}
		expireTime += rand.Int63n(15) // Avoid lots of keys being expired at the same time
		err = conn.Send("EXPIRE", key, expireTime)
	}

	// Link with its upstream job if upstream job ID exists for future querying
	if !utils.IsEmptyStr(stats.Info.UpstreamJobID) {
		k := rds.KeyUpstreamJobAndExecutions(bt.namespace, stats.Info.UpstreamJobID)
		zargs := []interface{}{k, "NX", stats.Info.RunAt, stats.Info.JobID}
		err = conn.Send("ZADD", zargs...)
	}

	// Check command send error only once here before executing
	if err != nil {
		return
	}

	_, err = conn.Do("EXEC")

	return
}

// UpdateStatusWithRetry updates the status with retry enabled.
// If update status failed, then retry if permitted.
// Try best to do
func (bt *basicTracker) UpdateStatusWithRetry(targetStatus Status) error {
	err := bt.compareAndSet(targetStatus)
	if err != nil {
		// Status mismatching error will be ignored
		if !errs.IsStatusMismatchError(err) {
			// Push to the retrying Q
			if er := bt.pushToQueueForRetry(targetStatus); er != nil {
				logger.Errorf("push job status update request to retry queue error: %s", er)
				// If failed to put it into the retrying Q in case, let's downgrade to retry in current process
				// by recursively call in goroutines.
				bt.retryUpdateStatus(targetStatus)
			}
		}
	}

	return err
}

// Refresh the job stats in mem
func (bt *basicTracker) refresh(targetStatus Status, checkIn ...string) {
	now := time.Now().Unix()

	bt.jobStats.Info.Status = targetStatus.String()
	if len(checkIn) > 0 {
		bt.jobStats.Info.CheckIn = checkIn[0]
		bt.jobStats.Info.CheckInAt = now
	}
	bt.jobStats.Info.UpdateTime = now
}

// FireHookEvent fires the hook event
func (bt *basicTracker) fireHookEvent(status Status, checkIn ...string) error {
	// Check if hook URL is registered
	if utils.IsEmptyStr(bt.jobStats.Info.WebHookURL) {
		// Do nothing
		return nil
	}

	change := &StatusChange{
		JobID:    bt.jobID,
		Status:   status.String(),
		Metadata: bt.jobStats.Info,
	}

	if len(checkIn) > 0 {
		change.CheckIn = checkIn[0]
	}

	// If callback is registered, then trigger now
	if bt.callback != nil {
		return bt.callback(bt.jobStats.Info.WebHookURL, change)
	}

	return nil
}

func (bt *basicTracker) pushToQueueForRetry(targetStatus Status) error {
	simpleStatusChange := &SimpleStatusChange{
		JobID:        bt.jobID,
		TargetStatus: targetStatus.String(),
	}

	rawJSON, err := json.Marshal(simpleStatusChange)
	if err != nil {
		return err
	}

	conn := bt.pool.Get()
	defer conn.Close()

	key := rds.KeyStatusUpdateRetryQueue(bt.namespace)
	args := []interface{}{key, "NX", time.Now().Unix(), rawJSON}

	_, err = conn.Do("ZADD", args...)

	return err
}

func (bt *basicTracker) retryUpdateStatus(targetStatus Status) {
	go func() {
		select {
		case <-time.After(time.Duration(5)*time.Minute + time.Duration(rand.Int31n(13))*time.Second):
			// Check the update timestamp
			if time.Now().Unix()-bt.jobStats.Info.UpdateTime < statDataExpireTime-24*3600 {
				if err := bt.compareAndSet(targetStatus); err != nil {
					logger.Errorf("Retry to update job status error: %s", err)
					bt.retryUpdateStatus(targetStatus)
				}
				// Success
			}
			return
		case <-bt.context.Done():
			return // terminated
		}
	}()
}

func (bt *basicTracker) compareAndSet(targetStatus Status) error {
	conn := bt.pool.Get()
	defer conn.Close()

	rootKey := rds.KeyJobStats(bt.namespace, bt.jobID)

	st, err := getStatus(conn, rootKey)
	if err != nil {
		return err
	}

	diff := st.Compare(targetStatus)
	if diff > 0 {
		return errs.StatusMismatchError(st.String(), targetStatus.String())
	}
	if diff == 0 {
		// Desired matches actual
		return nil
	}

	return setStatus(conn, rootKey, targetStatus)
}

// retrieve the stats of job tracked by this tracker from the backend data
func (bt *basicTracker) retrieve() error {
	conn := bt.pool.Get()
	defer conn.Close()

	key := rds.KeyJobStats(bt.namespace, bt.jobID)
	vals, err := redis.Strings(conn.Do("HGETALL", key))
	if err != nil {
		return err
	}

	if vals == nil || len(vals) == 0 {
		return errs.NoObjectFoundError(bt.jobID)
	}

	res := &Stats{
		Info: &StatsInfo{},
	}

	for i, l := 0, len(vals); i < l; i = i + 2 {
		prop := vals[i]
		value := vals[i+1]
		switch prop {
		case "id":
			res.Info.JobID = value
			break
		case "name":
			res.Info.JobName = value
			break
		case "kind":
			res.Info.JobKind = value
		case "unique":
			v, err := strconv.ParseBool(value)
			if err != nil {
				v = false
			}
			res.Info.IsUnique = v
		case "status":
			res.Info.Status = value
			break
		case "ref_link":
			res.Info.RefLink = value
			break
		case "enqueue_time":
			v, _ := strconv.ParseInt(value, 10, 64)
			res.Info.EnqueueTime = v
			break
		case "update_time":
			v, _ := strconv.ParseInt(value, 10, 64)
			res.Info.UpdateTime = v
			break
		case "run_at":
			v, _ := strconv.ParseInt(value, 10, 64)
			res.Info.RunAt = v
			break
		case "check_in_at":
			v, _ := strconv.ParseInt(value, 10, 64)
			res.Info.CheckInAt = v
			break
		case "check_in":
			res.Info.CheckIn = value
			break
		case "cron_spec":
			res.Info.CronSpec = value
			break
		case "web_hook_url":
			res.Info.WebHookURL = value
			break
		case "die_at":
			v, _ := strconv.ParseInt(value, 10, 64)
			res.Info.DieAt = v
		case "upstream_job_id":
			res.Info.UpstreamJobID = value
			break
		case "numeric_policy_id":
			v, _ := strconv.ParseInt(value, 10, 64)
			res.Info.NumericPID = v
			break
		case "parameters":
			params := make(Parameters)
			if err := json.Unmarshal([]byte(value), &params); err == nil {
				res.Info.Parameters = params
			}
			break
		default:
			break
		}
	}

	bt.jobStats = res

	return nil
}

func getStatus(conn redis.Conn, key string) (Status, error) {
	values, err := rds.HmGet(conn, key, "status")
	if err != nil {
		return "", err
	}

	if len(values) == 1 {
		st := Status(values[0].([]byte))
		if st.Validate() == nil {
			return st, nil
		}
	}

	return "", errors.New("malformed status data returned")
}

func setStatus(conn redis.Conn, key string, status Status) error {
	return rds.HmSet(conn, key, "status", status.String(), "update_time", time.Now().Unix())
}

// queryExecutions queries periodic executions by status
func queryExecutions(conn redis.Conn, dataKey string, q *query.Parameter) ([]string, int64, error) {
	total, err := redis.Int64(conn.Do("ZCOUNT", dataKey, 0, "+inf"))
	if err != nil {
		return nil, 0, err
	}

	var pageNumber, pageSize uint = 1, query.DefaultPageSize
	if q.PageNumber > 0 {
		pageNumber = q.PageNumber
	}
	if q.PageSize > 0 {
		pageSize = q.PageSize
	}

	results := make([]string, 0)
	if total == 0 || (int64)((pageNumber-1)*pageSize) >= total {
		return results, total, nil
	}

	offset := (pageNumber - 1) * pageSize
	args := []interface{}{dataKey, "+inf", 0, "LIMIT", offset, pageSize}

	eIDs, err := redis.Values(conn.Do("ZREVRANGEBYSCORE", args...))
	if err != nil {
		return nil, 0, err
	}

	for _, eID := range eIDs {
		if eIDBytes, ok := eID.([]byte); ok {
			results = append(results, string(eIDBytes))
		}
	}

	return results, total, nil
}
