package service

import (
    "github.com/ouqiang/gocron/models"
    "io/ioutil"
    "net/http"
    "strconv"
    "time"
    "github.com/ouqiang/gocron/modules/logger"
    "github.com/ouqiang/gocron/modules/ssh"
    "github.com/jakecoffman/cron"
    "github.com/ouqiang/gocron/modules/utils"
    "errors"
    "fmt"
)

var Cron *cron.Cron
var runInstance Instance

// 任务ID作为Key, 不会出现并发读写, 不加锁
type Instance struct {
    Status map[int]bool
}

// 是否有任务处于运行中
func (i *Instance) has(key int) bool {
    running, ok := i.Status[key]
    if ok && running {
        return true
    }

    return false
}

func (i *Instance) add(key int)  {
    i.Status[key] = true
}

func (i *Instance) done(key int)  {
    i.Status[key] = false
}

type Task struct{}

type TaskResult struct {
    Result string
    Err error
    RetryTimes int8
}

// 初始化任务, 从数据库取出所有任务, 添加到定时任务并运行
func (task *Task) Initialize() {
    Cron = cron.New()
    Cron.Start()
    runInstance = Instance{make(map[int]bool)}
    taskModel := new(models.Task)
    taskList, err := taskModel.ActiveList()
    if err != nil {
        logger.Error("获取任务列表错误-", err.Error())
        return
    }
    if len(taskList) == 0 {
        logger.Debug("任务列表为空")
        return
    }
    task.BatchAdd(taskList)
}

// 批量添加任务
func (task *Task) BatchAdd(tasks []models.TaskHost)  {
    for _, item := range tasks {
        task.Add(item)
    }
}

// 添加任务
func (task *Task) Add(taskModel models.TaskHost) {
    taskFunc := createJob(taskModel)
    if taskFunc == nil {
        logger.Error("创建任务处理Job失败,不支持的任务协议#", taskModel.Protocol)
        return
    }

    cronName := strconv.Itoa(taskModel.Id)
    // Cron任务采用数组存储, 删除任务需遍历数组, 并对数组重新赋值, 任务较多时，有性能问题
    Cron.RemoveJob(cronName)
    err := Cron.AddFunc(taskModel.Spec, taskFunc, cronName)
    if err != nil {
        logger.Error("添加任务到调度器失败#", err)
    }
}

// 直接运行任务
func (task *Task) Run(taskModel models.TaskHost)  {
    go createJob(taskModel)()
}

type Handler interface {
    Run(taskModel models.TaskHost) (string, error)
}

// 本地命令
type LocalCommandHandler struct {}

// 运行本地命令
func (h *LocalCommandHandler) Run(taskModel models.TaskHost) (string, error)  {
    if taskModel.Command == "" {
        return "", errors.New("invalid command")
    }

    if utils.IsWindows() {
        return h.runOnWindows(taskModel)
    }

    return h.runOnUnix(taskModel)
}

// 执行Windows命令
func (h *LocalCommandHandler) runOnWindows(taskModel models.TaskHost) (string, error) {
    outputGBK, err := utils.ExecShellWithTimeout(taskModel.Timeout, "cmd", "/C", taskModel.Command)
    // windows平台编码为gbk，需转换为utf8才能入库
    outputUTF8, ok := utils.GBK2UTF8(outputGBK)
    if ok {
        return outputUTF8, err
    }

    return "命令输出转换编码失败(gbk to utf8)", err
}

// 执行Unix命令
func (h *LocalCommandHandler) runOnUnix(taskModel models.TaskHost) (string, error)  {
    return utils.ExecShellWithTimeout(taskModel.Timeout, "/bin/bash", "-c", taskModel.Command)
}

// HTTP任务
type HTTPHandler struct{}

func (h *HTTPHandler) Run(taskModel models.TaskHost) (result string, err error) {
    client := &http.Client{}
    if taskModel.Timeout > 0 {
        client.Timeout = time.Duration(taskModel.Timeout) * time.Second
    }
    req, err := http.NewRequest("GET", taskModel.Command, nil)
    if err != nil {
        logger.Error("任务处理#创建HTTP请求错误-", err.Error())
        return
    }
    req.Header.Set("Content-type", "application/x-www-form-urlencoded")
    req.Header.Set("User-Agent", "golang/gocron")

    resp, err := client.Do(req)
    if err != nil {
        logger.Error("任务处理HTTP请求错误-", err.Error())
        return
    }
    defer resp.Body.Close()
    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        logger.Error("任务处理#读取HTTP请求返回值失败-", err.Error())
        return
    }
    // 返回状态码非200，均为失败
    if resp.StatusCode != 200 {
        return string(body), errors.New(fmt.Sprintf("HTTP状态码非200-->%d", resp.StatusCode))
    }

    return string(body), err
}

// SSH-command任务
type SSHCommandHandler struct{}

func (h *SSHCommandHandler) Run(taskModel models.TaskHost) (string, error) {
    sshConfig := ssh.SSHConfig{
        User: taskModel.Username,
        Password: taskModel.Password,
        Host: taskModel.Name,
        Port: taskModel.Port,
        ExecTimeout: taskModel.Timeout,
        AuthType: taskModel.AuthType,
        PrivateKey: taskModel.PrivateKey,
    }
    return ssh.Exec(sshConfig, taskModel.Command)
}

// 创建任务日志
func createTaskLog(taskModel models.TaskHost, status models.Status) (int64, error) {
    taskLogModel := new(models.TaskLog)
    taskLogModel.TaskId = taskModel.Id
    taskLogModel.Name = taskModel.Task.Name
    taskLogModel.Spec = taskModel.Spec
    taskLogModel.Protocol = taskModel.Protocol
    taskLogModel.Command = taskModel.Command
    taskLogModel.Timeout = taskModel.Timeout
    if taskModel.Protocol == models.TaskSSH {
        taskLogModel.Hostname = taskModel.Alias + "-" + taskModel.Name
    }
    taskLogModel.StartTime = time.Now()
    taskLogModel.Status = status
    insertId, err := taskLogModel.Create()

    return insertId, err
}

// 更新任务日志
func updateTaskLog(taskLogId int64, taskResult TaskResult) (int64, error) {
    taskLogModel := new(models.TaskLog)
    var status models.Status
    var result string = taskResult.Result
    if taskResult.Err != nil {
        result = taskResult.Err.Error() + " " + result
        status = models.Failure
    } else {
        status = models.Finish
    }
    return taskLogModel.Update(taskLogId, models.CommonMap{
        "retry_times": taskResult.RetryTimes,
        "status": status,
        "result": result,
    })

}

func createJob(taskModel models.TaskHost) cron.FuncJob {
    var handler Handler = createHandler(taskModel)
    if handler == nil {
        return nil
    }
    taskFunc := func() {
        if taskModel.Multi == 0 && runInstance.has(taskModel.Id) {
            createTaskLog(taskModel, models.Cancel)
            return
        }
        if taskModel.Multi == 0 {
            runInstance.add(taskModel.Id)
            defer runInstance.done(taskModel.Id)
        }
        taskLogId, err := createTaskLog(taskModel, models.Running)
        if err != nil {
            logger.Error("任务开始执行#写入任务日志失败-", err)
            return
        }
        taskResult := execJob(handler, taskModel)
        _, err = updateTaskLog(taskLogId, taskResult)
        if err != nil {
            logger.Error("任务结束#更新任务日志失败-", err)
        }
    }

    return taskFunc
}

func createHandler(taskModel models.TaskHost) Handler  {
    var handler Handler = nil
    switch taskModel.Protocol {
        case models.TaskHTTP:
            handler = new(HTTPHandler)
        case models.TaskSSH:
            handler = new(SSHCommandHandler)
        case models.TaskLocalCommand:
            handler = new(LocalCommandHandler)
    }

    return handler;
}

func execJob(handler Handler, taskModel models.TaskHost) TaskResult  {
    // 默认只运行任务一次
    var execTimes int8 = 1
    if (taskModel.RetryTimes > 0) {
        execTimes += taskModel.RetryTimes
    }
    var i int8 = 0
    var output string
    var err error
    for i < execTimes {
        output, err = handler.Run(taskModel)
        if err == nil {
            return TaskResult{Result: output, Err: err, RetryTimes: i}
        }
        i++
        if i < execTimes {
            logger.Warnf("任务执行失败#任务id-%d#重试第%d次#输出-%s#错误-%s", taskModel.Id, i, output, err.Error())
            // 重试间隔时间，每次递增1分钟
            time.Sleep( time.Duration(i) * time.Minute)
        }
    }

    return TaskResult{Result: output, Err: err, RetryTimes: taskModel.RetryTimes}
}