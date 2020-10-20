/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package hostsnap

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"configcenter/src/apimachinery/flowctrl"
	"configcenter/src/auth/extensions"
	"configcenter/src/common"
	"configcenter/src/common/auditoplog"
	"configcenter/src/common/backbone"
	"configcenter/src/common/blog"
	"configcenter/src/common/mapstr"
	"configcenter/src/common/metadata"
	"configcenter/src/common/util"
	"configcenter/src/scene_server/datacollection/app/options"
	dcUtil "configcenter/src/scene_server/datacollection/datacollection/middleware"
	"configcenter/src/storage/dal"

	"github.com/tidwall/gjson"
	"gopkg.in/redis.v5"
)

var (
	fetchDBInterval = time.Minute * 10
	// defaultRateLimiterQPS is the default value of rateLimiter qps
	defaultRateLimiterQPS   = 40
	// defaultRateLimiterBurst is the default value of rateLimiter burst
	defaultRateLimiterBurst = 100
	// defaultChangeRangePercent is the value of the default percentage of data fluctuation
	defaultChangeRangePercent = 10
	// minChangeRangePercent is the value of the minimum percentage of data fluctuation
	minChangeRangePercent = 1
)

type HostSnap struct {
	redisCli    *redis.Client
	authManager extensions.AuthManager
	httpHeader  http.Header
	*backbone.Engine
	rateLimit   flowctrl.RateLimiter
	changeRangePercent int
	cache     *Cache
	cachelock sync.RWMutex
	ctx       context.Context
	db        dal.RDB
}

type Cache struct {
	cache map[bool]*HostCache
	flag  bool
}

func NewHostSnap(ctx context.Context, redisCli *redis.Client, db dal.RDB, engine *backbone.Engine, authManager extensions.AuthManager, snapConfig options.HostSnap) *HostSnap {
	header := http.Header{}
	header.Add(common.BKHTTPOwnerID, common.BKDefaultOwnerID)
	header.Add(common.BKHTTPHeaderUser, common.CCSystemCollectorUserName)
	h := &HostSnap{
		redisCli:   redisCli,
		ctx:        ctx,
		db:         db,
		rateLimit:  getRateLimiter(snapConfig.QPS, snapConfig.Burst),
		changeRangePercent: getConfigIntVal(snapConfig.ChangeRangePercent,"hostsnap.changeRangePercentVal", defaultChangeRangePercent, minChangeRangePercent),
		httpHeader: header,
		cache: &Cache{
			cache: map[bool]*HostCache{},
			flag:  false,
		},
		authManager: authManager,
		Engine:      engine,
	}
	go h.fetchDBLoop()
	return h
}

func getRateLimiter(qps, burst string) flowctrl.RateLimiter {
	qpsVal := getConfigIntVal(qps, "hostsnap.qps", defaultRateLimiterQPS,1)
	burstVal := getConfigIntVal(burst, "hostsnap.burst", defaultRateLimiterBurst,1)
	return flowctrl.NewRateLimiter(int64(qpsVal), int64(burstVal))
}

// getConfigIntVal return the configured int value
func getConfigIntVal(configVal, configName string, defaultValue, minValue int) int {
	configIntVal, err := strconv.Atoi(configVal)
	if err != nil {
		blog.Errorf("can't find the value of %s settings, set the default value: %s", configName, defaultValue)
		configIntVal = defaultValue
	}
	if configIntVal < minValue {
		blog.Errorf("%s config %d smaller than minimum value, use minimum value %d", configName, configIntVal, minValue)
		configIntVal = minValue
	}
	return configIntVal
}

func (h *HostSnap) Analyze(mesg string) error {
	var data = mesg
	if !gjson.Get(mesg, "cloudid").Exists() {
		data = gjson.Get(mesg, "data").String()
	}
	val := gjson.Parse(data)
	host := h.getHostByVal(&val)
	if host == nil {
		blog.Warnf("[data-collection][hostsnap] host not found, continue, %s", val.String())
		return fmt.Errorf("host not found, continue, value: %s", val.String())
	}
	hostID := host.get(common.BKHostIDField)
	hostIdInt64, ok := hostID.(int64)
	if !ok {
		blog.Errorf("host id convert from interface to int64 failed, fail to update host, hostId:%v", hostID)
		return fmt.Errorf("host id convert from interface to int64 failed, fail to update host")
	}
	hostIdStr := strconv.FormatInt(hostIdInt64, 10)
	if hostIdStr == "" {
		blog.Warnf("[data-collection][hostsnap] host id not found, continue, %s", val.String())
		return fmt.Errorf("get host id failed, return, val: %s", val.String())
	}

	if err := h.redisCli.Set(common.RedisSnapKeyPrefix+hostIdStr, data, time.Minute*10).Err(); err != nil {
		blog.Errorf("[data-collection][hostsnap] save snapshot %s to redis failed: %s", common.RedisSnapKeyPrefix+hostIdStr, err.Error())
	}

	innerIp, ok := host.get(common.BKHostInnerIPField).(string)
	if !ok {
		blog.Infof("[data-collection][hostsnap] innerip is empty, continue, %s", val.String())
		return fmt.Errorf("get host innerIp failed")
	}
	outIp, ok := host.get(common.BKHostOuterIPField).(string)
	if !ok {
		blog.Warnf("[data-collection][hostsnap] outerip is not string, %s", val.String())
	}
	setter := parseSetter(&val, innerIp, outIp)
	// no need to update
	if !h.needToUpdate(setter, host) {
		return nil
	}

	// limit the number of requests
	if !h.rateLimit.TryAccept() {
		blog.Warnf("skip host snapshot data update due to request limit, host id: %d, ip: %s", hostID, innerIp)
		return nil
	}

	cond := mapstr.New()
	cond.Set(common.BKHostIDField, hostID)
	opt := &metadata.UpdateOption{
		Condition: cond,
		Data:      mapstr.NewFromMap(setter),
	}
	res, err := h.CoreAPI.CoreService().Instance().UpdateInstance(h.ctx, h.httpHeader, common.BKInnerObjIDHost, opt)
	if err != nil {
		blog.Errorf("Update host http do error, err: %v,input:%+v,param:%+v", err, data, opt)
		return err
	}
	if !res.Result {
		blog.Errorf("failed to update host, error msg: %v", res.ErrMsg)
		return fmt.Errorf("UpdateInstacne http response error,err code: %d, err msg: %v, opt: %v", res.Code, res.ErrMsg, opt)
	}

	preData := host.clone()
	copyVal(setter, host)

	// add auditLog
	curData, err := h.CoreAPI.CoreService().Host().GetHostByID(h.ctx, h.httpHeader, hostIdStr)
	if err != nil {
		blog.Errorf("GetHostByID http request failed, err: %s, hostID: %s", err, hostIdStr)
		return err
	}
	if !curData.Result {
		blog.Errorf("GetHostByID http response error, err code:%d, err msg: %s, hostID: %s", curData.Code, curData.ErrMsg, hostIdStr)
		return err
	}
	input := &metadata.HostModuleRelationRequest{HostIDArr: []int64{hostIdInt64}}
	moduleHost, err := h.CoreAPI.CoreService().Host().GetHostModuleRelation(h.ctx, h.httpHeader, input)
	if err != nil {
		blog.Errorf("[data-collection][hostsnap] GetConfigByCond http do error, err:%s, input:%+v", err.Error(), input)
		return err
	}
	if !moduleHost.Result {
		blog.Errorf("GetConfigByCond http response error, err code:%d, err msg:%s, input:%+v", moduleHost.Code, moduleHost.ErrMsg, input)
		return fmt.Errorf("[data-collection][hostsnap] get moduleHostConfig failed, fail to create auditLog")
	}

	auditHeader, err := dcUtil.GetAuditLogHeader(h.CoreAPI, h.httpHeader, common.BKInnerObjIDHost)
	if err != nil {
		blog.Errorf("GetAuditLogHeader failed, fail to create auditLog, objID: %s, err: %s", common.BKInnerObjIDHost, err.Error())
		return err
	}
	var bizID int64
	if len(moduleHost.Data.Info) > 0 {
		bizID = moduleHost.Data.Info[0].AppID
	}
	auditLog := metadata.SaveAuditLogParams{
		ID:    hostIdInt64,
		Model: common.BKInnerObjIDHost,
		Content: metadata.Content{
			CurData: curData.Data,
			PreData: preData,
			Headers: auditHeader,
		},
		OpDesc: "update " + common.BKInnerObjIDHost,
		OpType: auditoplog.AuditOpTypeModify,
		ExtKey: innerIp,
		BizID:  bizID,
	}
	result, err := h.CoreAPI.CoreService().Audit().SaveAuditLog(h.ctx, h.httpHeader, auditLog)
	if err != nil {
		blog.Errorf("create host audit log failed, http failed, err:%s", err.Error())
		return err
	}
	if !result.Result {
		blog.Errorf("create host audit log failed, err code:%d, err msg:%s", result.Code, result.ErrMsg)
		return fmt.Errorf("create host audit log failed, err msg: %s", result.ErrMsg)
	}

	return nil
}

func copyVal(a map[string]interface{}, b *HostInst) {
	for k, v := range a {
		b.set(k, v)
	}
}

func (h *HostSnap) needToUpdate(src map[string]interface{}, toCompare *HostInst) bool {
	for key, val := range src {
		compareVal := toCompare.get(key)
		if compareVal == nil && val == "" {
			continue
		}
		if compareVal != val {
			if key == "bk_cpu" || key == "bk_cpu_mhz" || key == "bk_disk" || key == "bk_mem" {
				srcFloat64Val, err := util.GetFloat64ByInterface(val)
				if err != nil {
					continue
				}
				compareFloat64Val, err := util.GetFloat64ByInterface(compareVal)
				if err != nil {
					continue
				}
				rangeVal := compareFloat64Val * (float64(h.changeRangePercent)/100.0)
				diff := srcFloat64Val - compareFloat64Val
				if -rangeVal < diff && diff < rangeVal {
					continue
				}
			}
			return true
		}
	}
	return false
}

func parseSetter(val *gjson.Result, innerIP, outerIP string) map[string]interface{} {
	var cpumodule = val.Get("data.cpu.cpuinfo.0.modelName").String()
	cpumodule = strings.TrimSpace(cpumodule)
	var cupnum int64
	for _, core := range val.Get("data.cpu.cpuinfo.#.cores").Array() {
		cupnum += core.Int()
	}
	var CPUMhz = val.Get("data.cpu.cpuinfo.0.mhz").Int()
	var disk uint64
	for _, disktotal := range val.Get("data.disk.usage.#.total").Array() {
		disk += disktotal.Uint() >> 10 >> 10 >>10
	}
	var mem = val.Get("data.mem.meminfo.total").Uint()
	var hostname = val.Get("data.system.info.hostname").String()
	hostname = strings.TrimSpace(hostname)
	var ostype = val.Get("data.system.info.os").String()
	ostype = strings.TrimSpace(ostype)
	var osname string
	platform := val.Get("data.system.info.platform").String()
	platform = strings.TrimSpace(platform)
	version := val.Get("data.system.info.platformVersion").String()
	switch strings.ToLower(ostype) {
	case "linux":
		version = strings.Replace(version, ".x86_64", "", 1)
		version = strings.Replace(version, ".i386", "", 1)
		osname = fmt.Sprintf("%s %s", ostype, platform)
		ostype = common.HostOSTypeEnumLinux
	case "windows":
		version = strings.Replace(version, "Microsoft ", "", 1)
		platform = strings.Replace(platform, "Microsoft ", "", 1)
		osname = fmt.Sprintf("%s", platform)
		ostype = common.HostOSTypeEnumWindows
	case "aix":
		osname = platform
		ostype = common.HostOSTypeEnumAIX
	default:
		osname = fmt.Sprintf("%s", platform)
	}
	version = strings.TrimSpace(version)
	osname = strings.TrimSpace(osname)
	var OuterMAC, InnerMAC string
	for _, inter := range val.Get("data.net.interface").Array() {
		for _, addr := range inter.Get("addrs.#.addr").Array() {
			ip := strings.Split(addr.String(), "/")[0]
			if ip == innerIP {
				InnerMAC = inter.Get("hardwareaddr").String()
				InnerMAC = strings.TrimSpace(InnerMAC)
			} else if ip == outerIP {
				OuterMAC = inter.Get("hardwareaddr").String()
				OuterMAC = strings.TrimSpace(OuterMAC)
			}
		}
	}

	osbit := val.Get("data.system.info.systemtype").String()
	osbit = strings.TrimSpace(osbit)

	dockerClientVersion := val.Get("data.system.docker.Client.Version").String()
	dockerClientVersion = strings.TrimSpace(dockerClientVersion)
	dockerServerVersion := val.Get("data.system.docker.Server.Version").String()
	dockerServerVersion = strings.TrimSpace(dockerServerVersion)

	mem = mem >> 10 >> 10
	setter := map[string]interface{}{
		"bk_cpu":                            cupnum,
		"bk_cpu_module":                     cpumodule,
		"bk_cpu_mhz":                        CPUMhz,
		"bk_disk":                           disk,
		"bk_mem":                            mem,
		"bk_os_type":                        ostype,
		"bk_os_name":                        osname,
		"bk_os_version":                     version,
		"bk_host_name":                      hostname,
		"bk_outer_mac":                      OuterMAC,
		"bk_mac":                            InnerMAC,
		"bk_os_bit":                         osbit,
		common.HostFieldDockerClientVersion: dockerClientVersion,
		common.HostFieldDockerServerVersion: dockerServerVersion,
	}

	if cupnum <= 0 {
		blog.Infof("bk_cpu not found in message for %s", innerIP)
	}
	if cpumodule == "" {
		blog.Infof("bk_cpu_module not found in message for %s", innerIP)
	}
	if CPUMhz <= 0 {
		blog.Infof("bk_cpu_mhz not found in message for %s", innerIP)
	}
	if disk <= 0 {
		blog.Infof("bk_disk not found in message for %s", innerIP)
	}
	if mem <= 0 {
		blog.Infof("bk_mem not found in message for %s", innerIP)
	}
	if ostype == "" {
		blog.Infof("bk_os_type not found in message for %s", innerIP)
	}
	if osname == "" {
		blog.Infof("bk_os_name not found in message for %s", innerIP)
	}
	if version == "" {
		blog.Infof("bk_os_version not found in message for %s", innerIP)
	}
	if hostname == "" {
		blog.Infof("bk_host_name not found in message for %s", innerIP)
	}
	if outerIP != "" && OuterMAC == "" {
		blog.Infof("bk_outer_mac not found in message for %s", innerIP)
	}
	if InnerMAC == "" {
		blog.Infof("bk_mac not found in message for %s", innerIP)
	}

	return setter
}

func (h *HostSnap) getHostByVal(val *gjson.Result) *HostInst {
	cloudid := val.Get("cloudid").String()
	ownerID := val.Get("bizid").String()

	ips := getIPS(val)
	if len(ips) > 0 {
		blog.Infof("[data-collection][hostsnap] handle clouid: %s ips: %v", cloudid, ips)
		for _, ip := range ips {
			if host := h.getCache().get(cloudid + "::" + ip); host != nil {
				return host
			}
		}

		blog.Infof("[data-collection][hostsnap] ips not in cache clouid: %s,ip: %v", cloudid, ips)
		cloudIDInt, err := strconv.Atoi(cloudid)
		if nil != err {
			blog.Infof("[data-collection][hostsnap] cloudid \"%s\" not integer", cloudid)
			return nil
		}
		condition := map[string]interface{}{
			common.BKCloudIDField: cloudIDInt,
			common.BKHostInnerIPField: map[string]interface{}{
				common.BKDBIN: ips,
			},
			common.BKOwnerIDField: ownerID,
		}
		result := make([]map[string]interface{}, 0)
		err = h.db.Table(common.BKTableNameBaseHost).Find(condition).All(h.ctx, &result)
		if err != nil {
			blog.Errorf("[data-collection][hostsnap] fetch db error %v", err)
		}
		for index := range result {
			cloudID := fmt.Sprint(result[index][common.BKCloudIDField])
			innerIP := fmt.Sprint(result[index][common.BKHostInnerIPField])
			inst := &HostInst{data: result[index]}
			h.setCache(cloudID+"::"+innerIP, inst)
			return inst
		}
		blog.Infof("[data-collection][hostsnap] ips not in cache and db, clouid: %v, ip: %v", cloudid, ips)
	} else {
		blog.Errorf("[data-collection][hostsnap] message has no ip, message:%s", val.String())
	}
	return nil
}

func getIPS(val *gjson.Result) (ips []string) {
	if !strings.HasPrefix(val.Get("ip").String(), "127.0.0.") {
		ips = append(ips, val.Get("ip").String())
	}
	interfaces := val.Get("data.net.interface.#.addrs.#.addr").Array()
	for _, addrs := range interfaces {
		for _, addr := range addrs.Array() {
			ip := strings.Split(addr.String(), "/")[0]
			if strings.HasPrefix(ip, "127.0.0.") {
				continue
			}
			ips = append(ips, ip)
		}
	}
	return ips
}

func (h *HostSnap) getCache() *HostCache {
	h.cachelock.RLock()
	defer h.cachelock.RUnlock()
	return h.cache.cache[h.cache.flag]
}

func (h *HostSnap) setCache(key string, val *HostInst) {
	h.cachelock.Lock()
	h.cache.cache[h.cache.flag].set(key, val)
	h.cachelock.Unlock()
}

func (h *HostSnap) fetchDBLoop() {
	h.cachelock.Lock()
	h.cache.cache[h.cache.flag] = h.fetch()
	h.cachelock.Unlock()

	for range time.Tick(fetchDBInterval) {
		cache := h.fetch()
		h.cachelock.Lock()
		h.cache.cache[!h.cache.flag] = cache
		h.cache.flag = !h.cache.flag
		h.cachelock.Unlock()
	}
}

func (h *HostSnap) fetch() *HostCache {
	hostcache := &HostCache{data: map[string]*HostInst{}}

	const limit = uint64(1000)
	var start = uint64(0)
	for {
		result := make([]map[string]interface{}, 0)
		err := h.db.Table(common.BKTableNameBaseHost).Find(nil).Start(start).Limit(limit).All(h.ctx, &result)
		if err != nil {
			blog.Errorf("[data-collection][hostsnap] fetch db error %v", err)
		}
		for index := range result {
			cloudid := fmt.Sprint(result[index][common.BKCloudIDField])
			innerip := fmt.Sprint(result[index][common.BKHostInnerIPField])
			hostcache.data[cloudid+"::"+innerip] = &HostInst{data: result[index]}
		}
		if uint64(len(result)) < limit {
			break
		}
		start += limit
	}
	blog.Infof("[data-collection][hostsnap] success fetch %d collections to cache", len(hostcache.data))
	return hostcache
}

type HostInst struct {
	sync.RWMutex
	data map[string]interface{}
}

func (h *HostInst) get(key string) interface{} {
	h.RLock()
	value := h.data[key]
	h.RUnlock()
	return value
}

func (h *HostInst) clone() map[string]interface{} {
	cloneHost := make(map[string]interface{})
	h.RLock()
	for key, value := range h.data {
		cloneHost[key] = value
	}
	h.RUnlock()
	return cloneHost
}

func (h *HostInst) set(key string, value interface{}) {
	h.Lock()
	h.data[key] = value
	h.Unlock()
}

type HostCache struct {
	sync.RWMutex
	data map[string]*HostInst
}

func (h *HostCache) get(key string) *HostInst {
	h.RLock()
	value := h.data[key]
	h.RUnlock()
	return value
}

func (h *HostCache) set(key string, value *HostInst) {
	h.Lock()
	h.data[key] = value
	h.Unlock()
}

const MockMessage = "{\"localTime\": \"2017-09-19 16:57:00\", \"data\": \"{\\\"ip\\\":\\\"192.168.1.7\\\",\\\"bizid\\\":0,\\\"cloudid\\\":0,\\\"data\\\":{\\\"timezone\\\":8,\\\"datetime\\\":\\\"2017-09-19 16:57:07\\\",\\\"utctime\\\":\\\"2017-09-19 08:57:07\\\",\\\"country\\\":\\\"Asia\\\",\\\"city\\\":\\\"Shanghai\\\",\\\"cpu\\\":{\\\"cpuinfo\\\":[{\\\"cpu\\\":0,\\\"vendorID\\\":\\\"GenuineIntel\\\",\\\"family\\\":\\\"6\\\",\\\"model\\\":\\\"63\\\",\\\"stepping\\\":2,\\\"physicalID\\\":\\\"0\\\",\\\"coreID\\\":\\\"0\\\",\\\"cores\\\":1,\\\"modelName\\\":\\\"Intel(R) Xeon(R) CPU E5-26xx v3\\\",\\\"mhz\\\":2294.01,\\\"cacheSize\\\":4096,\\\"flags\\\":[\\\"fpu\\\",\\\"vme\\\",\\\"de\\\",\\\"pse\\\",\\\"tsc\\\",\\\"msr\\\",\\\"pae\\\",\\\"mce\\\",\\\"cx8\\\",\\\"apic\\\",\\\"sep\\\",\\\"mtrr\\\",\\\"pge\\\",\\\"mca\\\",\\\"cmov\\\",\\\"pat\\\",\\\"pse36\\\",\\\"clflush\\\",\\\"mmx\\\",\\\"fxsr\\\",\\\"sse\\\",\\\"sse2\\\",\\\"ss\\\",\\\"ht\\\",\\\"syscall\\\",\\\"nx\\\",\\\"lm\\\",\\\"constant_tsc\\\",\\\"up\\\",\\\"rep_good\\\",\\\"unfair_spinlock\\\",\\\"pni\\\",\\\"pclmulqdq\\\",\\\"ssse3\\\",\\\"fma\\\",\\\"cx16\\\",\\\"pcid\\\",\\\"sse4_1\\\",\\\"sse4_2\\\",\\\"x2apic\\\",\\\"movbe\\\",\\\"popcnt\\\",\\\"tsc_deadline_timer\\\",\\\"aes\\\",\\\"xsave\\\",\\\"avx\\\",\\\"f16c\\\",\\\"rdrand\\\",\\\"hypervisor\\\",\\\"lahf_lm\\\",\\\"abm\\\",\\\"xsaveopt\\\",\\\"bmi1\\\",\\\"avx2\\\",\\\"bmi2\\\"],\\\"microcode\\\":\\\"1\\\"}],\\\"per_usage\\\":[3.0232169701043103],\\\"total_usage\\\":3.0232169701043103,\\\"per_stat\\\":[{\\\"cpu\\\":\\\"cpu0\\\",\\\"user\\\":5206.09,\\\"system\\\":6107.04,\\\"idle\\\":337100.84,\\\"nice\\\":6.68,\\\"iowait\\\":528.24,\\\"irq\\\":0.02,\\\"softirq\\\":13.48,\\\"steal\\\":0,\\\"guest\\\":0,\\\"guestNice\\\":0,\\\"stolen\\\":0}],\\\"total_stat\\\":{\\\"cpu\\\":\\\"cpu-total\\\",\\\"user\\\":5206.09,\\\"system\\\":6107.04,\\\"idle\\\":337100.84,\\\"nice\\\":6.68,\\\"iowait\\\":528.24,\\\"irq\\\":0.02,\\\"softirq\\\":13.48,\\\"steal\\\":0,\\\"guest\\\":0,\\\"guestNice\\\":0,\\\"stolen\\\":0}},\\\"env\\\":{\\\"crontab\\\":[{\\\"user\\\":\\\"root\\\",\\\"content\\\":\\\"#secu-tcs-agent monitor, install at Fri Sep 15 16:12:02 CST 2017\\\\n* * * * * /usr/local/sa/agent/secu-tcs-agent-mon-safe.sh /usr/local/sa/agent \\\\u003e /dev/null 2\\\\u003e\\\\u00261\\\\n*/1 * * * * /usr/local/qcloud/stargate/admin/start.sh \\\\u003e /dev/null 2\\\\u003e\\\\u00261 \\\\u0026\\\\n*/20 * * * * /usr/sbin/ntpdate ntpupdate.tencentyun.com \\\\u003e/dev/null \\\\u0026\\\\n*/1 * * * * cd /usr/local/gse/gseagent; ./cron_agent.sh 1\\\\u003e/dev/null 2\\\\u003e\\\\u00261\\\\n\\\"}],\\\"host\\\":\\\"127.0.0.1  localhost  localhost.localdomain  VM_0_31_centos\\\\n::1         localhost localhost.localdomain localhost6 localhost6.localdomain6\\\\n\\\",\\\"route\\\":\\\"Kernel IP routing table\\\\nDestination     Gateway         Genmask         Flags Metric Ref    Use Iface\\\\n10.0.0.0        0.0.0.0         255.255.255.0   U     0      0        0 eth0\\\\n169.254.0.0     0.0.0.0         255.255.0.0     U     1002   0        0 eth0\\\\n0.0.0.0         10.0.0.1        0.0.0.0         UG    0      0        0 eth0\\\\n\\\"},\\\"disk\\\":{\\\"diskstat\\\":{\\\"vda1\\\":{\\\"major\\\":252,\\\"minor\\\":1,\\\"readCount\\\":24347,\\\"mergedReadCount\\\":570,\\\"writeCount\\\":696357,\\\"mergedWriteCount\\\":4684783,\\\"readBytes\\\":783955968,\\\"writeBytes\\\":22041231360,\\\"readSectors\\\":1531164,\\\"writeSectors\\\":43049280,\\\"readTime\\\":80626,\\\"writeTime\\\":12704736,\\\"iopsInProgress\\\":0,\\\"ioTime\\\":822057,\\\"weightedIoTime\\\":12785026,\\\"name\\\":\\\"vda1\\\",\\\"serialNumber\\\":\\\"\\\",\\\"speedIORead\\\":0,\\\"speedByteRead\\\":0,\\\"speedIOWrite\\\":2.9,\\\"speedByteWrite\\\":171144.53333333333,\\\"util\\\":0.0025666666666666667,\\\"avgrq_sz\\\":115.26436781609195,\\\"avgqu_sz\\\":0.06568333333333334,\\\"await\\\":22.649425287356323,\\\"svctm\\\":0.8850574712643678}},\\\"partition\\\":[{\\\"device\\\":\\\"/dev/vda1\\\",\\\"mountpoint\\\":\\\"/\\\",\\\"fstype\\\":\\\"ext3\\\",\\\"opts\\\":\\\"rw,noatime,acl,user_xattr\\\"}],\\\"usage\\\":[{\\\"path\\\":\\\"/\\\",\\\"fstype\\\":\\\"ext2/ext3\\\",\\\"total\\\":52843638784,\\\"free\\\":47807447040,\\\"used\\\":2351915008,\\\"usedPercent\\\":4.4507060113962345,\\\"inodesTotal\\\":3276800,\\\"inodesUsed\\\":29554,\\\"inodesFree\\\":3247246,\\\"inodesUsedPercent\\\":0.9019165039062501}]},\\\"load\\\":{\\\"load_avg\\\":{\\\"load1\\\":0,\\\"load5\\\":0,\\\"load15\\\":0}},\\\"mem\\\":{\\\"meminfo\\\":{\\\"total\\\":1044832256,\\\"available\\\":805912576,\\\"used\\\":238919680,\\\"usedPercent\\\":22.866797864249705,\\\"free\\\":92041216,\\\"active\\\":521183232,\\\"inactive\\\":352964608,\\\"wired\\\":0,\\\"buffers\\\":110895104,\\\"cached\\\":602976256,\\\"writeback\\\":0,\\\"dirty\\\":151552,\\\"writebacktmp\\\":0},\\\"vmstat\\\":{\\\"total\\\":0,\\\"used\\\":0,\\\"free\\\":0,\\\"usedPercent\\\":0,\\\"sin\\\":0,\\\"sout\\\":0}},\\\"net\\\":{\\\"interface\\\":[{\\\"mtu\\\":65536,\\\"name\\\":\\\"lo\\\",\\\"hardwareaddr\\\":\\\"28:31:52:1d:c6:0a\\\",\\\"flags\\\":[\\\"up\\\",\\\"loopback\\\"],\\\"addrs\\\":[{\\\"addr\\\":\\\"127.0.0.1/8\\\"}]},{\\\"mtu\\\":1500,\\\"name\\\":\\\"eth0\\\",\\\"hardwareaddr\\\":\\\"52:54:00:19:2e:e8\\\",\\\"flags\\\":[\\\"up\\\",\\\"broadcast\\\",\\\"multicast\\\"],\\\"addrs\\\":[{\\\"addr\\\":\\\"127.0.0.1/24\\\"}]}],\\\"dev\\\":[{\\\"name\\\":\\\"lo\\\",\\\"speedSent\\\":0,\\\"speedRecv\\\":0,\\\"speedPacketsSent\\\":0,\\\"speedPacketsRecv\\\":0,\\\"bytesSent\\\":604,\\\"bytesRecv\\\":604,\\\"packetsSent\\\":2,\\\"packetsRecv\\\":2,\\\"errin\\\":0,\\\"errout\\\":0,\\\"dropin\\\":0,\\\"dropout\\\":0,\\\"fifoin\\\":0,\\\"fifoout\\\":0},{\\\"name\\\":\\\"eth0\\\",\\\"speedSent\\\":574,\\\"speedRecv\\\":214,\\\"speedPacketsSent\\\":3,\\\"speedPacketsRecv\\\":2,\\\"bytesSent\\\":161709123,\\\"bytesRecv\\\":285910298,\\\"packetsSent\\\":1116625,\\\"packetsRecv\\\":1167796,\\\"errin\\\":0,\\\"errout\\\":0,\\\"dropin\\\":0,\\\"dropout\\\":0,\\\"fifoin\\\":0,\\\"fifoout\\\":0}],\\\"netstat\\\":{\\\"established\\\":2,\\\"syncSent\\\":1,\\\"synRecv\\\":0,\\\"finWait1\\\":0,\\\"finWait2\\\":0,\\\"timeWait\\\":0,\\\"close\\\":0,\\\"closeWait\\\":0,\\\"lastAck\\\":0,\\\"listen\\\":2,\\\"closing\\\":0},\\\"protocolstat\\\":[{\\\"protocol\\\":\\\"udp\\\",\\\"stats\\\":{\\\"inDatagrams\\\":176253,\\\"inErrors\\\":0,\\\"noPorts\\\":1,\\\"outDatagrams\\\":199569,\\\"rcvbufErrors\\\":0,\\\"sndbufErrors\\\":0}}]},\\\"system\\\":{\\\"info\\\":{\\\"hostname\\\":\\\"VM_0_31_centos\\\",\\\"uptime\\\":348315,\\\"bootTime\\\":1505463112,\\\"procs\\\":142,\\\"os\\\":\\\"linux\\\",\\\"platform\\\":\\\"centos\\\",\\\"platformFamily\\\":\\\"rhel\\\",\\\"platformVersion\\\":\\\"6.2\\\",\\\"kernelVersion\\\":\\\"2.6.32-504.30.3.el6.x86_64\\\",\\\"virtualizationSystem\\\":\\\"\\\",\\\"virtualizationRole\\\":\\\"\\\",\\\"hostid\\\":\\\"96D0F4CA-2157-40E6-BF22-6A7CD9B6EB8C\\\",\\\"systemtype\\\":\\\"64-bit\\\"}}}}\", \"timestamp\": 1505811427, \"dtEventTime\": \"2017-09-19 16:57:07\", \"dtEventTimeStamp\": 1505811427000}"

const MockMessageData = `{
    "ip": "192.168.1.7",
    "bizid": 0,
    "cloudid": 0,
    "data": {
        "timezone": 8,
        "datetime": "2017-09-19 16:57:07",
        "utctime": "2017-09-19 08:57:07",
        "country": "Asia",
        "city": "Shanghai",
        "cpu": {
            "cpuinfo": [
                {
                    "cpu": 0,
                    "vendorID": "GenuineIntel",
                    "family": "6",
                    "model": "63",
                    "stepping": 2,
                    "physicalID": "0",
                    "coreID": "0",
                    "cores": 1,
                    "modelName": "Intel(R) Xeon(R) CPU E5-26xx v3",
                    "mhz": 2294.01,
                    "cacheSize": 4096,
                    "flags": [
                        "fpu",
                        "vme",
                        "de",
                        "pse",
                        "tsc",
                        "msr",
                        "pae",
                        "mce",
                        "cx8",
                        "apic",
                        "sep",
                        "mtrr",
                        "pge",
                        "mca",
                        "cmov",
                        "pat",
                        "pse36",
                        "clflush",
                        "mmx",
                        "fxsr",
                        "sse",
                        "sse2",
                        "ss",
                        "ht",
                        "syscall",
                        "nx",
                        "lm",
                        "constant_tsc",
                        "up",
                        "rep_good",
                        "unfair_spinlock",
                        "pni",
                        "pclmulqdq",
                        "ssse3",
                        "fma",
                        "cx16",
                        "pcid",
                        "sse4_1",
                        "sse4_2",
                        "x2apic",
                        "movbe",
                        "popcnt",
                        "tsc_deadline_timer",
                        "aes",
                        "xsave",
                        "avx",
                        "f16c",
                        "rdrand",
                        "hypervisor",
                        "lahf_lm",
                        "abm",
                        "xsaveopt",
                        "bmi1",
                        "avx2",
                        "bmi2"
                    ],
                    "microcode": "1"
                }
            ],
            "per_usage": [
                3.0232169701043103
            ],
            "total_usage": 3.0232169701043103,
            "per_stat": [
                {
                    "cpu": "cpu0",
                    "user": 5206.09,
                    "system": 6107.04,
                    "idle": 337100.84,
                    "nice": 6.68,
                    "iowait": 528.24,
                    "irq": 0.02,
                    "softirq": 13.48,
                    "steal": 0,
                    "guest": 0,
                    "guestNice": 0,
                    "stolen": 0
                }
            ],
            "total_stat": {
                "cpu": "cpu-total",
                "user": 5206.09,
                "system": 6107.04,
                "idle": 337100.84,
                "nice": 6.68,
                "iowait": 528.24,
                "irq": 0.02,
                "softirq": 13.48,
                "steal": 0,
                "guest": 0,
                "guestNice": 0,
                "stolen": 0
            }
        },
        BKHostOuterIPField       "env": {
            "crontab": [
                {
                    "user": "root",
                    "content": "#secu-tcs-agent monitor, install at Fri Sep 15 16:12:02 CST 2017\n* * * * * /usr/local/sa/agent/secu-tcs-agent-mon-safe.sh /usr/local/sa/agent > /dev/null 2>&1\n*/1 * * * * /usr/local/qcloud/stargate/admin/start.sh > /dev/null 2>&1 &\n*/20 * * * * /usr/sbin/ntpdate ntpupdate.tencentyun.com >/dev/null &\n*/1 * * * * cd /usr/local/gse/gseagent; ./cron_agent.sh 1>/dev/null 2>&1\n"
                }
            ],
            "host": "127.0.0.1  localhost  localhost.localdomain  VM_0_31_centos\n::1         localhost localhost.localdomain localhost6 localhost6.localdomain6\n",
            "route": "Kernel IP routing table\nDestination     Gateway         Genmask         Flags Metric Ref    Use Iface\n10.0.0.0        0.0.0.0         255.255.255.0   U     0      0        0 eth0\n169.254.0.0     0.0.0.0         255.255.0.0     U     1002   0        0 eth0\n0.0.0.0         10.0.0.1        0.0.0.0         UG    0      0        0 eth0\n"
        },
        "disk": {
            "diskstat": {
                "vda1": {
                    "major": 252,
                    "minor": 1,
                    "readCount": 24347,
                    "mergedReadCount": 570,
                    "writeCount": 696357,
                    "mergedWriteCount": 4684783,
                    "readBytes": 783955968,
                    "writeBytes": 22041231360,
                    "readSectors": 1531164,
                    "writeSectors": 43049280,
                    "readTime": 80626,
                    "writeTime": 12704736,
                    "iopsInProgress": 0,
                    "ioTime": 822057,
                    "weightedIoTime": 12785026,
                    "name": "vda1",
                    "serialNumber": "",
                    "speedIORead": 0,
                    "speedByteRead": 0,
                    "speedIOWrite": 2.9,
                    "speedByteWrite": 171144.53333333333,
                    "util": 0.0025666666666666667,
                    "avgrq_sz": 115.26436781609195,
                    "avgqu_sz": 0.06568333333333334,
                    "await": 22.649425287356323,
                    "svctm": 0.8850574712643678
                }
            },
            "partition": [
                {
                    "device": "/dev/vda1",
                    "mountpoint": "/",
                    "fstype": "ext3",
                    "opts": "rw,noatime,acl,user_xattr"
                }
            ],
            "usage": [
                {
                    "path": "/",
                    "fstype": "ext2/ext3",
                    "total": 52843638784,
                    "free": 47807447040,
                    "used": 2351915008,
                    "usedPercent": 4.4507060113962345,
                    "inodesTotal": 3276800,
                    "inodesUsed": 29554,
                    "inodesFree": 3247246,
                    "inodesUsedPercent": 0.9019165039062501
                }
            ]
        },
        "load": {
            "load_avg": {
                "load1": 0,
                "load5": 0,
                "load15": 0
            }
        },
        "mem": {
            "meminfo": {
                "total": 1044832256,
                "available": 805912576,
                "used": 238919680,
                "usedPercent": 22.866797864249705,
                "free": 92041216,
                "active": 521183232,
                "inactive": 352964608,
                "wired": 0,
                "buffers": 110895104,
                "cached": 602976256,
                "writeback": 0,
                "dirty": 151552,
                "writebacktmp": 0
            },
            "vmstat": {
                "total": 0,
                "used": 0,
                "free": 0,
                "usedPercent": 0,
                "sin": 0,
                "sout": 0
            }
        },
        "net": {
            "interface": [
                {
                    "mtu": 65536,
                    "name": "lo",
                    "hardwareaddr": "28:31:52:1d:c6:0a",
                    "flags": [
                        "up",
                        "loopback"
                    ],
                    "addrs": [
                        {
                            "addr": "127.0.0.1/8"
                        }
                    ]
                },
                {
                    "mtu": 1500,
                    "name": "eth0",
                    "hardwareaddr": "52:54:00:19:2e:e8",
                    "flags": [
                        "up",
                        "broadcast",
                        "multicast"
                    ],
                    "addrs": [
                        {
                            "addr": "127.0.0.1/24"
                        }
                    ]
                }
            ],
            "dev": [
                {
                    "name": "lo",
                    "speedSent": 0,
                    "speedRecv": 0,
                    "speedPacketsSent": 0,
                    "speedPacketsRecv": 0,
                    "bytesSent": 604,
                    "bytesRecv": 604,
                    "packetsSent": 2,
                    "packetsRecv": 2,
                    "errin": 0,
                    "errout": 0,
                    "dropin": 0,
                    "dropout": 0,
                    "fifoin": 0,
                    "fifoout": 0
                },
                {
                    "name": "eth0",
                    "speedSent": 574,
                    "speedRecv": 214,
                    "speedPacketsSent": 3,
                    "speedPacketsRecv": 2,
                    "bytesSent": 161709123,
                    "bytesRecv": 285910298,
                    "packetsSent": 1116625,
                    "packetsRecv": 1167796,
                    "errin": 0,
                    "errout": 0,
                    "dropin": 0,
                    "dropout": 0,
                    "fifoin": 0,
                    "fifoout": 0
                }
            ],
            "netstat": {
                "established": 2,
                "syncSent": 1,
                "synRecv": 0,
                "finWait1": 0,
                "finWait2": 0,
                "timeWait": 0,
                "close": 0,
                "closeWait": 0,
                "lastAck": 0,
                "listen": 2,
                "closing": 0
            },
            "protocolstat": [
                {
                    "protocol": "udp",
                    "stats": {
                        "inDatagrams": 176253,
                        "inErrors": 0,
                        "noPorts": 1,
                        "outDatagrams": 199569,
                        "rcvbufErrors": 0,
                        "sndbufErrors": 0
                    }
                }
            ]
        },
        "system": {
            "info": {
                "hostname": "VM_0_31_centos",
                "uptime": 348315,
                "bootTime": 1505463112,
                "procs": 142,
                "os": "linux",
                "platform": "centos",
                "platformFamily": "rhel",
                "platformVersion": "6.2",
                "kernelVersion": "2.6.32-504.30.3.el6.x86_64",
                "virtualizationSystem": "",
                "virtualizationRole": "",
                "hostid": "96D0F4CA-2157-40E6-BF22-6A7CD9B6EB8C",
                "systemtype": "64-bit"
            }
        }
    }
}`
