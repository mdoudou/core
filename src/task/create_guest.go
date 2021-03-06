package task

import (
	"github.com/project-nano/framework"
	"log"
	"time"
	"github.com/project-nano/core/modules"
	"net/http"
	"fmt"
	"strconv"
	"errors"
)

type CreateGuestExecutor struct {
	Sender         framework.MessageSender
	ResourceModule modules.ResourceModule
	Client         *http.Client
}

func (executor *CreateGuestExecutor)Execute(id framework.SessionID, request framework.Message,
	incoming chan framework.Message, terminate chan bool) (err error) {
	var config modules.InstanceStatus
	var guestName string
	if guestName, err = request.GetString(framework.ParamKeyName); err != nil{
		return err
	}

	if config.User, err = request.GetString(framework.ParamKeyUser); err != nil{
		return err
	}
	if config.Group, err = request.GetString(framework.ParamKeyGroup); err != nil{
		return err
	}
	if config.Pool, err = request.GetString(framework.ParamKeyPool); err != nil{
		return err
	}

	if config.Cores, err = request.GetUInt(framework.ParamKeyCore); err != nil{
		return err
	}
	if config.Memory, err = request.GetUInt(framework.ParamKeyMemory); err != nil{
		return err
	}
	if config.Disks, err = request.GetUIntArray(framework.ParamKeyDisk); err != nil{
		return err
	}
	if 0 == len(config.Disks){
		err = errors.New("must specify disk size")
		return
	}
	var systemDiskSize = uint(config.Disks[0])
	if config.AutoStart, err = request.GetBoolean(framework.ParamKeyOption); err != nil{
		return err
	}
	var templateID string
	if templateID, err = request.GetString(framework.ParamKeyTemplate); err != nil{
		err = fmt.Errorf("get template id fail: %s", err.Error())
		return
	}else{
		var respChan = make(chan modules.ResourceResult, 1)
		executor.ResourceModule.GetSystemTemplate(templateID, respChan)
		var result = <- respChan
		if result.Error != nil{
			err = fmt.Errorf("get template fail: %s", result.Error)
			return
		}
		var t = result.Template
		config.System = t.OperatingSystem
		var currentAdmin string
		currentAdmin, _ = request.GetString(framework.ParamKeyAdmin)
		if "" == currentAdmin{
			request.SetString(framework.ParamKeyAdmin, t.Admin)
		}
		var options []uint64
		if options, err = t.ToOptions(); err != nil{
			err = fmt.Errorf("invalid template: %s", err.Error())
			return
		}
		request.SetUIntArray(framework.ParamKeyTemplate, options)
	}

	//QoS
	{
		priorityValue, _ := request.GetUInt(framework.ParamKeyPriority)
		config.CPUPriority = modules.PriorityEnum(priorityValue)
		if limitParameters, err := request.GetUIntArray(framework.ParamKeyLimit); err == nil{
			const (
				ReadSpeedOffset           = iota
				WriteSpeedOffset
				ReadIOPSOffset
				WriteIOPSOffset
				ReceiveOffset
				SendOffset
				ValidLimitParametersCount = 6
			)

			if ValidLimitParametersCount != len(limitParameters){
				err = fmt.Errorf("invalid QoS parameters count %d", len(limitParameters))
				return err
			}
			config.ReadSpeed = limitParameters[ReadSpeedOffset]
			config.WriteSpeed = limitParameters[WriteSpeedOffset]
			config.ReadIOPS = limitParameters[ReadIOPSOffset]
			config.WriteIOPS = limitParameters[WriteIOPSOffset]
			config.ReceiveSpeed = limitParameters[ReceiveOffset]
			config.SendSpeed = limitParameters[SendOffset]
		}
	}

	log.Printf("[%08X] request create guest '%s' from %s.[%08X]", id, guestName,
		request.GetSender(), request.GetFromSession())

	resp, _ := framework.CreateJsonMessage(framework.CreateGuestResponse)
	resp.SetToSession(request.GetFromSession())
	resp.SetFromSession(id)
	resp.SetSuccess(false)
	resp.SetTransactionID(request.GetTransactionID())

	if err = QualifyNormalName(guestName); err != nil{
		log.Printf("[%08X] invalid guest name '%s' : %s", id, guestName, err.Error())
		err = fmt.Errorf("invalid guest name '%s': %s", guestName, err.Error())
		resp.SetError(err.Error())
		return executor.Sender.SendMessage(resp, request.GetSender())
	}

	if err = QualifyNormalName(config.User); err != nil{
		log.Printf("[%08X] invalid owner name '%s' : %s", id, config.User, err.Error())
		err = fmt.Errorf("invalid owner name '%s': %s", config.User, err.Error())
		resp.SetError(err.Error())
		return executor.Sender.SendMessage(resp, request.GetSender())
	}
	if err = QualifyNormalName(config.Group); err != nil{
		log.Printf("[%08X] invalid group name '%s' : %s", id, config.Group, err.Error())
		err = fmt.Errorf("invalid group name '%s': %s", config.Group, err.Error())
		resp.SetError(err.Error())
		return executor.Sender.SendMessage(resp, request.GetSender())
	}
	config.Name = fmt.Sprintf("%s.%s", config.Group, guestName)
	request.SetString(framework.ParamKeyName, config.Name)

	if imageID, err := request.GetString(framework.ParamKeyImage); err == nil{
		//clone from image
		var respChan = make(chan modules.ResourceResult)
		var imageServer, mediaHost string
		var mediaPort int
		{
			executor.ResourceModule.GetImageServer(respChan)
			var result = <- respChan
			if result.Error != nil{
				log.Printf("[%08X] get image server fail: %s", id, result.Error.Error())
				resp.SetError(result.Error.Error())
				return executor.ResponseFail(resp, result.Error.Error(), request.GetSender())
			}
			imageServer = result.Name
			mediaHost = result.Host
			mediaPort = result.Port
		}
		{
			query, _ := framework.CreateJsonMessage(framework.GetDiskImageRequest)
			query.SetFromSession(id)
			query.SetString(framework.ParamKeyImage, imageID)
			if err = executor.Sender.SendMessage(query, imageServer); err != nil{
				log.Printf("[%08X] request get disk image fail: %s", id, err.Error())
				resp.SetError(err.Error())
				return executor.ResponseFail(resp, err.Error(), request.GetSender())
			}

			var imageName string
			var imageSize uint
			var imageCreated bool

			timer := time.NewTimer(modules.DefaultOperateTimeout)
			select{
			case queryResp := <- incoming:
				if !queryResp.IsSuccess(){
					log.Printf("[%08X] get image info fail: %s", id, queryResp.GetError())
					resp.SetError(queryResp.GetError())
					return executor.ResponseFail(resp, queryResp.GetError(), request.GetSender())
				}
				imageName, _ = queryResp.GetString(framework.ParamKeyName)
				imageSize, _ = queryResp.GetUInt(framework.ParamKeySize)
				imageCreated, _ = queryResp.GetBoolean(framework.ParamKeyEnable)

			case <- timer.C:
				//timeout
				log.Printf("[%08X] get image info timeout", id)
				resp.SetError("time out")
				return executor.ResponseFail(resp, err.Error(), request.GetSender())
			}

			if !imageCreated{
				err = fmt.Errorf("disk image '%s' not created", imageID)
				log.Printf("[%08X] get disk image fail: %s", id, err.Error())
				return executor.ResponseFail(resp, err.Error(), request.GetSender())
			}
			if imageSize > systemDiskSize{
				err = fmt.Errorf("source image (%.2f GB) larger than system disk (%.2f GB)", float64(imageSize)/(1 << 30), float64(systemDiskSize)/(1 << 30))
				log.Printf("[%08X] check image size fail: %s", err.Error())
				return executor.ResponseFail(resp, err.Error(), request.GetSender())
			}

			log.Printf("[%08X] clone disk image '%s'(%d MB) from server '%s'(%s:%d)", id, imageName, imageSize >> 20,
				imageServer, mediaHost, mediaPort)
			request.SetString(framework.ParamKeyHost, mediaHost)
			request.SetUInt(framework.ParamKeyPort, uint(mediaPort))
			request.SetUInt(framework.ParamKeySize, imageSize)
		}
	}

	{
		//allocate cell
		var respChan = make(chan modules.ResourceResult)
		executor.ResourceModule.AllocateInstance(config.Pool, config, respChan)
		result := <- respChan
		if result.Error != nil{
			log.Printf("[%08X] allocate resource fail: %s", id, result.Error.Error())
			return executor.ResponseFail(resp, result.Error.Error(), request.GetSender())
		}
		var instance = result.Instance
		config.ID = instance.ID
		config.Cell = instance.Cell
		log.Printf("[%08X] new id '%s', cell '%s' allocated", id, config.ID, config.Cell)
		request.SetStringArray(framework.ParamKeyAddress, []string{instance.InternalNetwork.AssignedAddress, instance.ExternalNetwork.AssignedAddress})
	}
	var fromSession = request.GetFromSession()
	{
		//redirect request
		request.SetFromSession(id)
		request.SetUIntArray(framework.ParamKeyMode, []uint64{modules.NetworkModePlain, modules.StorageModeLocal})
		request.SetString(framework.ParamKeyInstance, config.ID)

		if err = executor.Sender.SendMessage(request, config.Cell); err != nil{
			log.Printf("[%08X] redirect create guest to cell '%s' fail: %s", id, config.Cell, err.Error())
			executor.CancelResource(config.ID)
			return executor.ResponseFail(resp, err.Error(), request.GetSender())
		}

		timer := time.NewTimer(modules.DefaultOperateTimeout)
		select{
		case cellResp := <- incoming:
			if cellResp.IsSuccess(){
				log.Printf("[%08X] cell create guest success", id)
			}else{
				log.Printf("[%08X] cell create guest fail: %s", id, cellResp.GetError())
				executor.CancelResource(config.ID)
			}
			cellResp.SetFromSession(id)
			cellResp.SetToSession(fromSession)
			cellResp.SetTransactionID(request.GetTransactionID())
			//forward
			return executor.Sender.SendMessage(cellResp, request.GetSender())
		case <- timer.C:
			//timeout
			log.Printf("[%08X] wait create response timeout", id)
			executor.CancelResource(config.ID)
			return executor.ResponseFail(resp, "timeout", request.GetSender())
		}
	}
}

func (executor *CreateGuestExecutor)ResponseFail(resp framework.Message, err , target string) error{
	resp.SetSuccess(false)
	resp.SetError(err)
	return executor.Sender.SendMessage(resp, target)
}

func (executor *CreateGuestExecutor)CancelResource(id string) error{
	var respChan = make(chan error)
	executor.ResourceModule.DeallocateInstance(id, nil, respChan)
	err := <- respChan
	return err
}

func (executor *CreateGuestExecutor) getImageSize(id, host string, port int) (size uint64, err error){
	const (
		Protocol = "https"
		Resource = "disk_images"
		LengthHeaderName = "Content-Length"
	)
	var fileURL = fmt.Sprintf("%s://%s:%d/%s/%s/file/", Protocol, host, port, Resource, id)
	resp, err := executor.Client.Head(fileURL)
	if err != nil{
		return
	}
	defer resp.Body.Close()
	intValue, err := strconv.Atoi(resp.Header.Get(LengthHeaderName))
	if err != nil{
		err = fmt.Errorf("invalid length '%s'", resp.Header.Get(LengthHeaderName))
		return
	}
	return uint64(intValue), nil
}
