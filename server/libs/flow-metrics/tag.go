/*
 * Copyright (c) 2024 Yunshan Networks
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package flow_metrics

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"

	"github.com/deepflowio/deepflow/server/libs/ckdb"
	"github.com/deepflowio/deepflow/server/libs/datatype"
	"github.com/deepflowio/deepflow/server/libs/datatype/prompb"
	"github.com/deepflowio/deepflow/server/libs/flow-metrics/pb"
	"github.com/deepflowio/deepflow/server/libs/pool"
	"github.com/deepflowio/deepflow/server/libs/utils"
	"github.com/google/gopacket/layers"
)

type Code uint64

const (
	IP Code = 0x1 << iota
	L3EpcID
	L3Device
	SubnetID
	RegionID
	PodNodeID
	HostID
	AZID
	PodGroupID
	PodNSID
	PodID
	MAC
	PodClusterID
	ServiceID
	Resource // 1<< 14
	GPID     // 1<< 15

	// Make sure the max offset <= 19
)

const (
	IPPath Code = 0x100000 << iota // 1 << 20
	L3EpcIDPath
	L3DevicePath
	SubnetIDPath
	RegionIDPath
	PodNodeIDPath
	HostIDPath
	AZIDPath
	PodGroupIDPath
	PodNSIDPath
	PodIDPath
	MACPath
	PodClusterIDPath
	ServiceIDPath
	ResourcePath // 1<< 34
	GPIDPath     // 1<< 35

	// Make sure the max offset <= 39
)

const (
	Direction Code = 0x10000000000 << iota // 1 << 40
	ACLGID
	Protocol
	ServerPort
	_
	TAPType
	_
	VTAPID
	TAPSide
	TAPPort
	IsKeyService
	L7Protocol // also represents AppService,AppInstance,EndPoint,BizType
	SignalSource
)

const (
	TunnelIPID Code = 1 << 62
)

func (c Code) HasEdgeTagField() bool {
	return c&0xfffff00000 != 0
}

type DeviceType uint8

const (
	_ DeviceType = iota
	VMDevice
	_
	ThirdPartyDevice // 3
	_
	VGatewayDevice // 5
	HostDevice
	NetworkDevice
	FloatingIPDevice
	DHCPDevice
)

type SideType uint8

const (
	NodeSide SideType = (iota + 1) << 3
	HypervisorSide
	GatewayHypervisorSide
	GatewaySide
	ProcessSide
	AppSide
)

type DirectionEnum uint8

const (
	_CLIENT_SERVER_MASK = 0x7
	_SIDE_TYPE_MASK     = 0xf8
)

const (
	ClientToServer = 1 << iota
	ServerToClient
	LocalToLocal

	// 以下类型为转换tapside而增加，在写入db时均记为c2s或s2c
	ClientNodeToServer              = ClientToServer | DirectionEnum(NodeSide)              // 客户端容器节点，路由、SNAT、隧道
	ServerNodeToClient              = ServerToClient | DirectionEnum(NodeSide)              // 服务端容器节点，路由、SNAT、隧道
	ClientHypervisorToServer        = ClientToServer | DirectionEnum(HypervisorSide)        // 客户端宿主机，隧道
	ServerHypervisorToClient        = ServerToClient | DirectionEnum(HypervisorSide)        // 服务端宿主机，隧道
	ClientGatewayHypervisorToServer = ClientToServer | DirectionEnum(GatewayHypervisorSide) // 客户端网关宿主机
	ServerGatewayHypervisorToClient = ServerToClient | DirectionEnum(GatewayHypervisorSide) // 服务端网关宿主机
	ClientGatewayToServer           = ClientToServer | DirectionEnum(GatewaySide)           // 客户端网关（特指VIP机制的SLB，例如微软云MUX等）, Mac地址对应的接口为vip设备
	ServerGatewayToClient           = ServerToClient | DirectionEnum(GatewaySide)           // 服务端网关（特指VIP机制的SLB，例如微软云MUX等）, Mac地址对应的接口为vip设备
	ClientProcessToServer           = ClientToServer | DirectionEnum(ProcessSide)           // 客户端进程
	ServerProcessToClient           = ServerToClient | DirectionEnum(ProcessSide)           // 服务端进程
	ClientAppToServer               = ClientToServer | DirectionEnum(AppSide)               // 客户端应用
	ServerAppToClient               = ServerToClient | DirectionEnum(AppSide)               // 服务端应用
)

func (d DirectionEnum) IsClientToServer() bool {
	return d&_CLIENT_SERVER_MASK == ClientToServer
}

func (d DirectionEnum) IsServerToClient() bool {
	return d&_CLIENT_SERVER_MASK == ServerToClient
}

func (d DirectionEnum) IsGateway() bool {
	return SideType(d&_SIDE_TYPE_MASK)&(GatewaySide|GatewayHypervisorSide) != 0
}

func (d DirectionEnum) ToRole() uint8 {
	switch d & _CLIENT_SERVER_MASK {
	case ClientToServer:
		return ROLE_CLIENT
	case ServerToClient:
		return ROLE_SERVER
	case LocalToLocal:
		return ROLE_LOCAL
	default:
		return ROLE_REST
	}
}

type TAPSideEnum uint8

const (
	Client TAPSideEnum = 1 << iota
	Server
	Local
	ClientNode              = Client | TAPSideEnum(NodeSide)
	ServerNode              = Server | TAPSideEnum(NodeSide)
	ClientHypervisor        = Client | TAPSideEnum(HypervisorSide)
	ServerHypervisor        = Server | TAPSideEnum(HypervisorSide)
	ClientGatewayHypervisor = Client | TAPSideEnum(GatewayHypervisorSide)
	ServerGatewayHypervisor = Server | TAPSideEnum(GatewayHypervisorSide)
	ClientGateway           = Client | TAPSideEnum(GatewaySide)
	ServerGateway           = Server | TAPSideEnum(GatewaySide)
	ClientProcess           = Client | TAPSideEnum(ProcessSide)
	ServerProcess           = Server | TAPSideEnum(ProcessSide)
	ClientApp               = Client | TAPSideEnum(AppSide)
	ServerApp               = Server | TAPSideEnum(AppSide)
	App                     = TAPSideEnum(AppSide)
	Rest                    = 0
)

const (
	ROLE_CLIENT = 0
	ROLE_SERVER = 1
	ROLE_LOCAL  = 2
	ROLE_REST   = 3
)

var TAPSideEnumsString = []string{
	Rest:                    "rest",
	Client:                  "c",
	Server:                  "s",
	Local:                   "local",
	ClientNode:              "c-nd",
	ServerNode:              "s-nd",
	ClientHypervisor:        "c-hv",
	ServerHypervisor:        "s-hv",
	ClientGatewayHypervisor: "c-gw-hv",
	ServerGatewayHypervisor: "s-gw-hv",
	ClientGateway:           "c-gw",
	ServerGateway:           "s-gw",
	ClientProcess:           "c-p",
	ServerProcess:           "s-p",
	ClientApp:               "c-app",
	ServerApp:               "s-app",
	App:                     "app",
}

func (s TAPSideEnum) String() string {
	return TAPSideEnumsString[s]
}

func (d DirectionEnum) ToTAPSide() TAPSideEnum {
	return TAPSideEnum(d)
}

// TAP: Traffic Access Point
//
// Indicates the flow data collection location.  Currently supports 255
// acquisition locations. The traffic in cloud is uniformly represented by
// a special value `3`, and the other values represent the traffic
// collected from optical splitting and mirroring at different locations
// in the IDC.
//
// Note: For historical reasons, we use the confusing term VTAP to refer
// to deepflow-agent, and agent_id to represent the id of a deepflow-agent.
type TAPTypeEnum uint8

const (
	IDC_MIN TAPTypeEnum = 1 // 1~2, 4~255: IDC
	CLOUD   TAPTypeEnum = 3
)

type TagSource uint8

const (
	GpId  TagSource = 1 << iota // if the GpId exists but the podId does not exist, first obtain the podId through the GprocessId table delivered by the Controller
	PodId                       // use vtapId + podId to match first
	Mac                         // if vtapId + podId cannot be matched, finally use Mac/EpcIP to match resources
	EpcIP
	Peer                // Multicast, filled with peer information
	Agent               // traffic on the 'lo' port uses the Agent's IP and Epc to match resource information.
	ProcessId           // If ProcessId exists and GpId does not exist, get GpId through ProcessId
	None      TagSource = 0
)

type Field struct {
	// 注意字节对齐！

	// 用于区分不同的trident及其不同的pipeline，用于如下场景：
	//   - agent和ingester之间的数据传输
	//   - ingester写入clickhouse，作用类似_id，序列化为_tid
	GlobalThreadID uint8 `json:"thread_id" category:"$tag"`

	// structTag  "datasource":"n|nm|a|am" means datasource: network, network_map, application, application_map
	IP6              net.IP `json:"ip6" map_json:"ip6_0" category:"$tag" sub:"network_layer" to_string:"IPv6String" ` // FIXME: 合并IP6和IP
	MAC              uint64
	IP               uint32     `json:"ip4" map_json:"ip4_0" category:"$tag" sub:"network_layer" to_string:"IPv4String"`
	L3EpcID          int32      `json:"l3_epc_id" map_json:"l3_epc_id_0" category:"$tag" sub:"universal_tag"`
	L3DeviceID       uint32     `json:"l3_device_id" map_json:"l3_device_id_0" category:"$tag" sub:"universal_tag"`
	L3DeviceType     DeviceType `json:"l3_device_type" map_json:"l3_device_type_0" category:"$tag" sub:"universal_tag"`
	RegionID         uint16     `json:"region_id" map_json:"region_id_0" category:"$tag" sub:"universal_tag"`
	SubnetID         uint16     `json:"subnet_id" map_json:"subnet_id_0" category:"$tag" sub:"universal_tag"`
	HostID           uint16     `json:"host_id" map_json:"host_id_0" category:"$tag" sub:"universal_tag"`
	PodNodeID        uint32     `json:"pod_node_id" map_json:"pod_node_id_0" category:"$tag" sub:"universal_tag"`
	AZID             uint16     `json:"az_id" map_json:"az_id_0" category:"$tag" sub:"universal_tag"`
	PodGroupID       uint32     `json:"pod_group_id" map_json:"pod_group_id_0" category:"$tag" sub:"universal_tag"`
	PodNSID          uint16     `json:"pod_ns_id" map_json:"pod_ns_id_0" category:"$tag" sub:"universal_tag"`
	PodID            uint32     `json:"pod_id" map_json:"pod_id_0" category:"$tag" sub:"universal_tag"`
	PodClusterID     uint16     `json:"pod_cluster_id" map_json:"pod_cluster_id_0" category:"$tag" sub:"universal_tag"`
	ServiceID        uint32     `json:"service_id" map_json:"service_id_0" category:"$tag" sub:"universal_tag"`
	AutoInstanceID   uint32     `json:"auto_instance_id" map_json:"auto_instance_id_0" category:"$tag" sub:"universal_tag"`
	AutoInstanceType uint8      `json:"auto_instance_type" map_json:"auto_instance_type_0" category:"$tag" sub:"universal_tag"`
	AutoServiceID    uint32     `json:"auto_service_id" map_json:"auto_service_id_0" category:"$tag" sub:"universal_tag"`
	AutoServiceType  uint8      `json:"auto_service_type" map_json:"auto_service_type_0" category:"$tag" sub:"universal_tag"`
	GPID             uint32     `json:"gprocess_id" map_json:"gprocess_id_0" category:"$tag" sub:"universal_tag"`

	MAC1              uint64
	IP61              net.IP     `json:"ip6_1" category:"$tag" sub:"network_layer" to_string:"IPv6String" datasource:"nm|am"` // FIXME: 合并IP61和IP1
	IP1               uint32     `json:"ip4_1" category:"$tag" sub:"network_layer" to_string:"IPv4String" datasource:"nm|am"`
	L3EpcID1          int32      `json:"l3_epc_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	L3DeviceID1       uint32     `json:"l3_device_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	L3DeviceType1     DeviceType `json:"l3_device_type_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	RegionID1         uint16     `json:"region_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	SubnetID1         uint16     `json:"subnet_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	HostID1           uint16     `json:"host_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	PodNodeID1        uint32     `json:"pod_node_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	AZID1             uint16     `json:"az_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	PodGroupID1       uint32     `json:"pod_group_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	PodNSID1          uint16     `json:"pod_ns_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	PodID1            uint32     `json:"pod_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	PodClusterID1     uint16     `json:"pod_cluster_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	ServiceID1        uint32     `json:"service_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	AutoInstanceID1   uint32     `json:"auto_instance_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	AutoInstanceType1 uint8      `json:"auto_instance_type_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	AutoServiceID1    uint32     `json:"auto_service_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	AutoServiceType1  uint8      `json:"auto_service_type_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`
	GPID1             uint32     `json:"gprocess_id_1" category:"$tag" sub:"universal_tag" datasource:"nm|am"`

	ACLGID     uint16
	Role       uint8             `json:"role" category:"$tag" sub:"capture_info" enumfile:"role" datasource:"n|a"`
	Protocol   layers.IPProtocol `json:"protocol" category:"$tag" sub:"network_layer" enumfile:"protocol"`
	ServerPort uint16            `json:"server_port" category:"$tag" sub:"transport_layer"`
	VTAPID     uint16            `json:"agent_id" category:"$tag" sub:"capture_info"`
	// Not stored, only determines which database to store in.
	// When Orgid is 0 or 1, it is stored in database 'flow_metrics', otherwise stored in '<OrgId>_flow_metrics'.
	OrgId   uint16 `json:"org_id" category:"$tag"`
	TeamID  uint16 `json:"team_id" category:"$tag"`
	TAPPort datatype.TapPort
	// caculate from TAPPort
	TapPort     uint32              `json:"capture_nic" category:"$tag" sub:"capture_info" datasource:"nm|am"`
	TapPortType uint8               `json:"capture_nic_type" category:"$tag" sub:"capture_info" enumfile:"capture_nic_type" datasource:"nm|am"`
	NatSource   datatype.NATSource  `json:"nat_source" category:"$tag" sub:"capture_info" enumfile:"nat_source" datasource:"nm|am"`
	TunnelType  datatype.TunnelType `json:"tunnel_type" category:"$tag" sub:"tunnel_info" enumfile:"tunnel_type" datasource:"nm|am"`

	TAPSide TAPSideEnum
	// only for exporters
	TAPSideStr   string      `json:"observation_point" category:"$tag" sub:"capture_info" enumfile:"observation_point" datasource:"nm|am"`
	TAPType      TAPTypeEnum `json:"capture_network_type_id" category:"$tag" sub:"capture_info"`
	IsIPv4       uint8       `json:"is_ipv4" category:"$tag" sub:"network_layer"` // (8B) 与IP/IP6是共生字段
	IsKeyService uint8
	L7Protocol   datatype.L7Protocol `json:"l7_protocol" category:"$tag" sub:"application_layer" enumfile:"l7_protocol" datasource:"a|am"`
	AppService   string              `json:"app_service" category:"$tag" sub:"service_info" datasource:"a|am"`
	AppInstance  string              `json:"app_instance" category:"$tag" sub:"service_info" datasource:"a|am"`
	Endpoint     string              `json:"endpoint" category:"$tag" sub:"service_info" datasource:"a|am"`
	BizType      uint8               `json:"biz_type" category:"$tag" sub:"capture_info" datasource:"a|am"`
	SignalSource uint16              `json:"signal_source" category:"$tag" sub:"capture_info" enumfile:"l7_signal_source"` // FIXME: network,network_1m should use l4_signal_source for translate

	TagSource, TagSource1 uint8

	TunnelIPID uint16
}

func newMetricsMinuteTable(id MetricsTableID, engine ckdb.EngineType, version, cluster, storagePolicy, ckdbType string, ttl int, coldStorage *ckdb.ColdStorage) *ckdb.Table {
	timeKey := "time"

	aggr1H1DTable := true
	var orderKeys []string
	code := metricsTableCodes[id]
	if code&L3EpcID != 0 {
		orderKeys = []string{timeKey, "l3_epc_id", "ip4", "ip6"}
	} else if code&L3EpcIDPath != 0 {
		orderKeys = []string{timeKey, "l3_epc_id_1", "ip4_1", "ip6_1", "l3_epc_id_0", "ip4_0", "ip6_0"}
	} else if code&ACLGID != 0 {
		orderKeys = []string{timeKey, "acl_gid"}
		aggr1H1DTable = false
	}
	if code&ServerPort != 0 {
		orderKeys = append(orderKeys, "server_port")
	}

	var meterColumns []*ckdb.Column
	switch id {
	case NETWORK_1M, NETWORK_MAP_1M:
		meterColumns = FlowMeterColumns()
	case TRAFFIC_POLICY_1M:
		meterColumns = UsageMeterColumns()
	case APPLICATION_1M, APPLICATION_MAP_1M:
		meterColumns = AppMeterColumns()
	}

	return &ckdb.Table{
		Version:         version,
		ID:              uint8(id),
		Database:        ckdb.METRICS_DB,
		DBType:          ckdbType,
		LocalName:       id.TableName() + ckdb.LOCAL_SUBFFIX,
		GlobalName:      id.TableName(),
		Columns:         append(GenTagColumns(metricsTableCodes[id]), meterColumns...),
		TimeKey:         timeKey,
		TTL:             ttl,
		PartitionFunc:   ckdb.TimeFuncTwelveHour,
		Engine:          engine,
		Cluster:         cluster,
		StoragePolicy:   storagePolicy,
		ColdStorage:     *coldStorage,
		OrderKeys:       orderKeys,
		PrimaryKeyCount: len(orderKeys),
		Aggr1H1D:        aggr1H1DTable,
	}
}

// 由分钟表生成秒表
func newMetricsSecondTable(minuteTable *ckdb.Table, ttl int, coldStorages *ckdb.ColdStorage) *ckdb.Table {
	t := *minuteTable
	t.ID = minuteTable.ID + uint8(NETWORK_1S)
	t.LocalName = MetricsTableID(t.ID).TableName() + ckdb.LOCAL_SUBFFIX
	t.GlobalName = MetricsTableID(t.ID).TableName()
	t.TTL = ttl
	t.ColdStorage = *coldStorages
	t.PartitionFunc = ckdb.TimeFuncFourHour
	t.Engine = ckdb.MergeTree // 秒级数据不用支持使用replica
	t.Aggr1H1D = false

	return &t
}

func GetMetricsTables(engine ckdb.EngineType, version, cluster, storagePolicy, ckdbType string, flowMinuteTtl, flowSecondTtl, appMinuteTtl, appSecondTtl int, coldStorages map[string]*ckdb.ColdStorage) []*ckdb.Table {
	var metricsTables []*ckdb.Table

	minuteTables := []*ckdb.Table{}
	for i := NETWORK_1M; i <= NETWORK_MAP_1M; i++ {
		minuteTables = append(minuteTables, newMetricsMinuteTable(i, engine, version, cluster, storagePolicy, ckdbType, flowMinuteTtl, ckdb.GetColdStorage(coldStorages, ckdb.METRICS_DB, i.TableName())))
	}
	for i := APPLICATION_1M; i <= APPLICATION_MAP_1M; i++ {
		minuteTables = append(minuteTables, newMetricsMinuteTable(i, engine, version, cluster, storagePolicy, ckdbType, appMinuteTtl, ckdb.GetColdStorage(coldStorages, ckdb.METRICS_DB, i.TableName())))
	}
	minuteTables = append(minuteTables, newMetricsMinuteTable(TRAFFIC_POLICY_1M, engine, version, cluster, storagePolicy, ckdbType, 3*24, ckdb.GetColdStorage(coldStorages, ckdb.METRICS_DB, TRAFFIC_POLICY_1M.TableName()))) // traffic_policy ttl is 3 day default

	secondTables := []*ckdb.Table{}
	for i := NETWORK_1S; i <= NETWORK_MAP_1S; i++ {
		secondTables = append(secondTables, newMetricsSecondTable(minuteTables[i-NETWORK_1S], flowSecondTtl, ckdb.GetColdStorage(coldStorages, ckdb.METRICS_DB, i.TableName())))
	}
	for i := APPLICATION_1S; i <= APPLICATION_MAP_1S; i++ {
		secondTables = append(secondTables, newMetricsSecondTable(minuteTables[i-NETWORK_1S], appSecondTtl, ckdb.GetColdStorage(coldStorages, ckdb.METRICS_DB, i.TableName())))
	}
	metricsTables = append(minuteTables, secondTables...)
	return metricsTables
}

type MetricsTableID uint8

const (
	NETWORK_1M MetricsTableID = iota
	NETWORK_MAP_1M

	APPLICATION_1M
	APPLICATION_MAP_1M

	TRAFFIC_POLICY_1M

	NETWORK_1S
	NETWORK_MAP_1S

	APPLICATION_1S
	APPLICATION_MAP_1S

	METRICS_TABLE_ID_MAX
)

func (i MetricsTableID) TableName() string {
	return metricsTableNames[i]
}

func (i MetricsTableID) TableCode() Code {
	return metricsTableCodes[i]
}

var metricsTableNames = []string{
	NETWORK_1M:     "network.1m",
	NETWORK_MAP_1M: "network_map.1m",

	APPLICATION_1M:     "application.1m",
	APPLICATION_MAP_1M: "application_map.1m",

	TRAFFIC_POLICY_1M: "traffic_policy.1m",

	NETWORK_1S:     "network.1s",
	NETWORK_MAP_1S: "network_map.1s",

	APPLICATION_1S:     "application.1s",
	APPLICATION_MAP_1S: "application_map.1s",
}

func MetricsTableNameToID(name string) MetricsTableID {
	for i, n := range metricsTableNames {
		if n == name {
			return MetricsTableID(i)
		}
	}
	return METRICS_TABLE_ID_MAX
}

const (
	BaseCode     = AZID | HostID | IP | L3Device | L3EpcID | PodClusterID | PodGroupID | PodID | PodNodeID | PodNSID | RegionID | SubnetID | TAPType | VTAPID | ServiceID | Resource | GPID | SignalSource
	BasePathCode = AZIDPath | HostIDPath | IPPath | L3DevicePath | L3EpcIDPath | PodClusterIDPath | PodGroupIDPath | PodIDPath | PodNodeIDPath | PodNSIDPath | RegionIDPath | SubnetIDPath | TAPSide | TAPType | VTAPID | ServiceIDPath | ResourcePath | GPIDPath | SignalSource
	BasePortCode = Protocol | ServerPort | IsKeyService

	NETWORK         = BaseCode | BasePortCode | Direction
	NETWORK_MAP     = BasePathCode | BasePortCode | TAPPort
	APPLICATION     = BaseCode | BasePortCode | Direction | L7Protocol
	APPLICATION_MAP = BasePathCode | BasePortCode | TAPPort | L7Protocol

	TRAFFIC_POLICY = ACLGID | TunnelIPID | VTAPID
)

var metricsTableCodes = []Code{
	NETWORK_1M:     NETWORK,
	NETWORK_MAP_1M: NETWORK_MAP,

	APPLICATION_1M:     APPLICATION,
	APPLICATION_MAP_1M: APPLICATION_MAP,

	TRAFFIC_POLICY_1M: TRAFFIC_POLICY,

	NETWORK_1S:     NETWORK,
	NETWORK_MAP_1S: NETWORK_MAP,

	APPLICATION_1S:     APPLICATION,
	APPLICATION_MAP_1S: APPLICATION_MAP,
}

type Tag struct {
	Field
	Code
	id string
}

func (t *Tag) ToKVString() string {
	buffer := make([]byte, MAX_STRING_LENGTH)
	size := t.MarshalTo(buffer)
	return string(buffer[:size])
}

const (
	ID_OTHER    = -1
	ID_INTERNET = -2
)

func marshalUint16WithSpecialID(v int16) string {
	switch v {
	case ID_OTHER:
		fallthrough
	case ID_INTERNET:
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatUint(uint64(v)&math.MaxUint16, 10)
}

func unmarshalUint16WithSpecialID(s string) (int16, error) {
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1, err
	}
	return int16(i), nil
}

func MarshalInt32WithSpecialID(v int32) int32 {
	if v > 0 || v == ID_OTHER || v == ID_INTERNET {
		return v
	}
	return int32(uint64(v) & math.MaxUint16)
}

func unmarshalInt32WithSpecialID(v int32) int16 {
	return int16(v)
}

func marshalUint16s(vs []uint16) string {
	var buf strings.Builder
	for i, v := range vs {
		buf.WriteString(strconv.FormatUint(uint64(v), 10))
		if i < len(vs)-1 {
			buf.WriteString("|")
		}
	}
	return buf.String()
}

func unmarshalUint16s(s string) ([]uint16, error) {
	uint16s := []uint16{}
	vs := strings.Split(s, "|")
	for _, v := range vs {
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return uint16s, err
		}
		uint16s = append(uint16s, uint16(i))
	}
	return uint16s, nil
}

// 注意: 必须要按tag字段的字典顺序进行处理
func (t *Tag) MarshalTo(b []byte) int {
	offset := 0

	// 在InfluxDB的line protocol中，tag紧跟在measurement name之后，总会以逗号开头
	if t.GlobalThreadID != 0 { // FIXME: zero写入的数据此字段总为0，目前无需该字段
		offset += copy(b[offset:], ",_tid=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.GlobalThreadID), 10))
	}

	if t.Code&ACLGID != 0 {
		offset += copy(b[offset:], ",acl_gid=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.ACLGID), 10))
	}
	if t.Code&AZID != 0 {
		offset += copy(b[offset:], ",az_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AZID), 10))
	}
	if t.Code&AZIDPath != 0 {
		offset += copy(b[offset:], ",az_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AZID), 10))
		offset += copy(b[offset:], ",az_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AZID1), 10))
	}

	if t.Code&Direction != 0 {
		if t.Role == ROLE_CLIENT {
			offset += copy(b[offset:], ",role=c2s")
		} else if t.Role == ROLE_SERVER {
			offset += copy(b[offset:], ",role=s2c")
		} else if t.Role == ROLE_LOCAL {
			offset += copy(b[offset:], ",role=local")
		} else {
			offset += copy(b[offset:], ",role=rest")
		}
	}
	if t.Code&GPID != 0 {
		offset += copy(b[offset:], ",gprocess_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.GPID), 10))
	}
	if t.Code&GPIDPath != 0 {
		offset += copy(b[offset:], ",gprocess_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.GPID), 10))
		offset += copy(b[offset:], ",gprocess_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.GPID1), 10))
	}
	if t.Code&HostID != 0 {
		offset += copy(b[offset:], ",host_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.HostID), 10))
	}
	if t.Code&HostIDPath != 0 {
		offset += copy(b[offset:], ",host_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.HostID), 10))
		offset += copy(b[offset:], ",host_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.HostID1), 10))
	}
	if t.Code&IP != 0 {
		if t.IsIPv4 == 0 {
			offset += copy(b[offset:], ",ip=")
			offset += copy(b[offset:], t.IP6.String())
			offset += copy(b[offset:], ",ip_version=6")
		} else {
			offset += copy(b[offset:], ",ip=")
			offset += copy(b[offset:], utils.IpFromUint32(t.IP).String())
			offset += copy(b[offset:], ",ip_version=4")
		}
	}
	if t.Code&IPPath != 0 {
		if t.IsIPv4 == 0 {
			offset += copy(b[offset:], ",ip_0=")
			offset += copy(b[offset:], t.IP6.String())
			offset += copy(b[offset:], ",ip_1=")
			offset += copy(b[offset:], t.IP61.String())
			offset += copy(b[offset:], ",is_version=6")
		} else {
			offset += copy(b[offset:], ",ip_0=")
			offset += copy(b[offset:], utils.IpFromUint32(t.IP).String())
			offset += copy(b[offset:], ",ip_1=")
			offset += copy(b[offset:], utils.IpFromUint32(t.IP1).String())
			offset += copy(b[offset:], ",ip_version=4")
		}
	}

	if t.Code&IsKeyService != 0 {
		if t.IsKeyService == 1 {
			offset += copy(b[offset:], ",is_key_service=1")
		} else {
			offset += copy(b[offset:], ",is_key_service=0")
		}
	}

	if t.Code&L3Device != 0 {
		offset += copy(b[offset:], ",l3_device_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L3DeviceID), 10))
		offset += copy(b[offset:], ",l3_device_type=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L3DeviceType), 10))
	}
	if t.Code&L3DevicePath != 0 {
		offset += copy(b[offset:], ",l3_device_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L3DeviceID), 10))
		offset += copy(b[offset:], ",l3_device_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L3DeviceID1), 10))
		offset += copy(b[offset:], ",l3_device_type_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L3DeviceType), 10))
		offset += copy(b[offset:], ",l3_device_type_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L3DeviceType1), 10))
	}
	if t.Code&L3EpcID != 0 {
		offset += copy(b[offset:], ",l3_epc_id=")
		offset += copy(b[offset:], strconv.FormatInt(int64(t.L3EpcID), 10))
	}
	if t.Code&L3EpcIDPath != 0 {
		offset += copy(b[offset:], ",l3_epc_id_0=")
		offset += copy(b[offset:], strconv.FormatInt(int64(t.L3EpcID), 10))
		offset += copy(b[offset:], ",l3_epc_id_1=")
		offset += copy(b[offset:], strconv.FormatInt(int64(t.L3EpcID1), 10))
	}
	if t.Code&L7Protocol != 0 {
		offset += copy(b[offset:], ",l7_protocol=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.L7Protocol), 10))
		offset += copy(b[offset:], ",app_service="+t.AppService)
		offset += copy(b[offset:], ",app_instance="+t.AppInstance)
		offset += copy(b[offset:], ",endpoint="+t.Endpoint)
		offset += copy(b[offset:], ",biz_type=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.BizType), 10))
	}
	if t.Code&MAC != 0 {
		// 不存入tsdb中
		//offset += copy(b[offset:], ",mac=")
		//offset += copy(b[offset:], utils.Uint64ToMac(t.MAC).String())
	}
	if t.Code&MACPath != 0 {
		// 不存入tsdb中
		//offset += copy(b[offset:], ",mac_0=")
		//offset += copy(b[offset:], utils.Uint64ToMac(t.MAC).String())
		//offset += copy(b[offset:], ",mac_1=")
		//offset += copy(b[offset:], utils.Uint64ToMac(t.MAC1).String())
	}

	if t.Code&PodClusterID != 0 {
		offset += copy(b[offset:], ",pod_cluster_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodClusterID), 10))
	}

	if t.Code&PodClusterIDPath != 0 {
		offset += copy(b[offset:], ",pod_cluster_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodClusterID), 10))
		offset += copy(b[offset:], ",pod_cluster_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodClusterID1), 10))
	}

	if t.Code&PodGroupID != 0 {
		offset += copy(b[offset:], ",pod_group_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodGroupID), 10))
	}

	if t.Code&PodGroupIDPath != 0 {
		offset += copy(b[offset:], ",pod_group_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodGroupID), 10))
		offset += copy(b[offset:], ",pod_group_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodGroupID1), 10))
	}

	if t.Code&PodID != 0 {
		offset += copy(b[offset:], ",pod_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodID), 10))
	}

	if t.Code&PodIDPath != 0 {
		offset += copy(b[offset:], ",pod_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodID), 10))
		offset += copy(b[offset:], ",pod_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodID1), 10))
	}

	if t.Code&PodNodeID != 0 {
		offset += copy(b[offset:], ",pod_node_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodNodeID), 10))
	}

	if t.Code&PodNodeIDPath != 0 {
		offset += copy(b[offset:], ",pod_node_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodNodeID), 10))
		offset += copy(b[offset:], ",pod_node_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodNodeID1), 10))
	}

	if t.Code&PodNSID != 0 {
		offset += copy(b[offset:], ",pod_ns_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodNSID), 10))
	}

	if t.Code&PodNSIDPath != 0 {
		offset += copy(b[offset:], ",pod_ns_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodNSID), 10))
		offset += copy(b[offset:], ",pod_ns_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.PodNSID1), 10))
	}

	if t.Code&Protocol != 0 {
		offset += copy(b[offset:], ",protocol=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.Protocol), 10))
	}

	if t.Code&RegionID != 0 {
		offset += copy(b[offset:], ",region_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.RegionID), 10))
	}
	if t.Code&RegionIDPath != 0 {
		offset += copy(b[offset:], ",region_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.RegionID), 10))
		offset += copy(b[offset:], ",region_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.RegionID1), 10))
	}

	if t.Code&Resource != 0 {
		offset += copy(b[offset:], ",auto_instance_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoInstanceID), 10))
		offset += copy(b[offset:], ",auto_instance_type=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoInstanceType), 10))
		offset += copy(b[offset:], ",auto_service_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoServiceID), 10))
		offset += copy(b[offset:], ",auto_service_type=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoServiceType), 10))
	}
	if t.Code&ResourcePath != 0 {
		offset += copy(b[offset:], ",auto_instance_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoInstanceID), 10))
		offset += copy(b[offset:], ",auto_instance_type_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoInstanceType), 10))
		offset += copy(b[offset:], ",auto_service_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoServiceID), 10))
		offset += copy(b[offset:], ",auto_service_type_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoServiceType), 10))

		offset += copy(b[offset:], ",auto_instance_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoInstanceID1), 10))
		offset += copy(b[offset:], ",auto_instance_type_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoInstanceType1), 10))
		offset += copy(b[offset:], ",auto_service_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoServiceID1), 10))
		offset += copy(b[offset:], ",auto_service_type_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.AutoServiceType1), 10))
	}

	if t.Code&SignalSource != 0 {
		offset += copy(b[offset:], ",signal_source=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.SignalSource), 10))
	}

	if t.Code&ServerPort != 0 {
		offset += copy(b[offset:], ",server_port=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.ServerPort), 10))
	}

	if t.Code&ServiceID != 0 {
		offset += copy(b[offset:], ",service_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.ServiceID), 10))
	}

	if t.Code&ServiceIDPath != 0 {
		offset += copy(b[offset:], ",service_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.ServiceID), 10))
		offset += copy(b[offset:], ",service_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.ServiceID1), 10))
	}

	if t.Code&SubnetID != 0 {
		offset += copy(b[offset:], ",subnet_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.SubnetID), 10))
	}
	if t.Code&SubnetIDPath != 0 {
		offset += copy(b[offset:], ",subnet_id_0=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.SubnetID), 10))
		offset += copy(b[offset:], ",subnet_id_1=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.SubnetID1), 10))
	}
	if t.Code&TunnelIPID != 0 {
		offset += copy(b[offset:], ",tunnel_ip_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.TunnelIPID), 10))
	}
	if t.Code&TAPPort != 0 {
		offset += copy(b[offset:], ",capture_nic=")
		offset += putTAPPort(b[offset:], uint64(t.TAPPort))
	}
	if t.Code&TAPSide != 0 {
		switch t.TAPSide {
		case Rest:
			offset += copy(b[offset:], ",observation_point=rest")
		case Client:
			offset += copy(b[offset:], ",observation_point=c")
		case Server:
			offset += copy(b[offset:], ",observation_point=s")
		case ClientNode:
			offset += copy(b[offset:], ",observation_point=c-nd")
		case ServerNode:
			offset += copy(b[offset:], ",observation_point=s-nd")
		case ClientHypervisor:
			offset += copy(b[offset:], ",observation_point=c-hv")
		case ServerHypervisor:
			offset += copy(b[offset:], ",observation_point=s-hv")
		case ClientGatewayHypervisor:
			offset += copy(b[offset:], ",observation_point=c-gw-hv")
		case ServerGatewayHypervisor:
			offset += copy(b[offset:], ",observation_point=s-gw-hv")
		case ClientGateway:
			offset += copy(b[offset:], ",observation_point=c-gw")
		case ServerGateway:
			offset += copy(b[offset:], ",observation_point=s-gw")
		}
	}
	if t.Code&TAPType != 0 {
		offset += copy(b[offset:], ",capture_network_type_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.TAPType), 10))
	}
	if t.Code&VTAPID != 0 {
		offset += copy(b[offset:], ",agent_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.VTAPID), 10))
		offset += copy(b[offset:], ",team_id=")
		offset += copy(b[offset:], strconv.FormatUint(uint64(t.TeamID), 10))
	}

	return offset
}

func (t *Tag) TableID(isSecond bool) (uint8, error) {
	for i, code := range metricsTableCodes {
		// 有时会有MAC,MACPath字段，需要先排除再比较
		if t.Code&^MAC&^MACPath == code {
			if isSecond {
				return uint8(i) + uint8(NETWORK_1S), nil
			}
			return uint8(i), nil
		}
	}
	return 0, fmt.Errorf("not match table, tag code is 0x%x is second %v", t.Code, isSecond)
}

func GenTagColumns(code Code) []*ckdb.Column {
	columns := []*ckdb.Column{}
	columns = append(columns, ckdb.NewColumnWithGroupBy("time", ckdb.DateTime))
	columns = append(columns, ckdb.NewColumn("_tid", ckdb.UInt8).SetComment("用于区分trident不同的pipeline").SetIndex(ckdb.IndexNone))
	if code&ACLGID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("acl_gid", ckdb.UInt16).SetComment("ACL组ID"))
	}
	if code&AZID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("az_id", ckdb.UInt16).SetComment("可用区ID"))
	}
	if code&AZIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("az_id_0", ckdb.UInt16).SetComment("ip4/6_0对应的可用区ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("az_id_1", ckdb.UInt16).SetComment("ip4/6_1对应的可用区ID"))
	}

	if code&Direction != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("role", ckdb.UInt8).SetComment("统计量对应的流方向. 0: ip为客户端, 1: ip为服务端, 2: ip为本地，3: 其他"))
	}

	if code&GPID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("gprocess_id", ckdb.UInt32).SetComment("全局进程ID"))
	}
	if code&GPIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("gprocess_id_0", ckdb.UInt32).SetComment("ip0对应的全局进程ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("gprocess_id_1", ckdb.UInt32).SetComment("ip1对应的全局进程ID"))
	}
	if code&HostID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("host_id", ckdb.UInt16).SetComment("宿主机ID"))
	}
	if code&HostIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("host_id_0", ckdb.UInt16).SetComment("ip4/6_0对应的宿主机ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("host_id_1", ckdb.UInt16).SetComment("ip4/6_1对应的宿主机ID"))
	}
	if code&IP != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("ip4", ckdb.IPv4).SetComment("IPv4地址"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("ip6", ckdb.IPv6).SetComment("IPV6地址"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("is_ipv4", ckdb.UInt8).SetIndex(ckdb.IndexMinmax).SetComment("是否IPV4地址. 0: 否, ip6字段有效, 1: 是, ip4字段有效"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("tag_source", ckdb.UInt8).SetComment("tag来源"))
	}
	if code&IPPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("ip4_0", ckdb.IPv4))
		columns = append(columns, ckdb.NewColumnWithGroupBy("ip4_1", ckdb.IPv4))
		columns = append(columns, ckdb.NewColumnWithGroupBy("ip6_0", ckdb.IPv6))
		columns = append(columns, ckdb.NewColumnWithGroupBy("ip6_1", ckdb.IPv6))
		columns = append(columns, ckdb.NewColumnWithGroupBy("is_ipv4", ckdb.UInt8).SetIndex(ckdb.IndexMinmax))
		columns = append(columns, ckdb.NewColumnWithGroupBy("tag_source_0", ckdb.UInt8).SetComment("ip_0对应的tag来源"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("tag_source_1", ckdb.UInt8).SetComment("ip_1对应的tag来源"))
	}

	if code&IsKeyService != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("is_key_service", ckdb.UInt8).SetComment("是否属于关键服务0: 否, 1: 是").SetIndex(ckdb.IndexMinmax))
	}

	if code&L3Device != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_device_id", ckdb.UInt32).SetComment("ip对应的资源ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_device_type", ckdb.UInt8).SetComment("ip对应的资源类型"))
	}

	if code&L3DevicePath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_device_id_0", ckdb.UInt32).SetComment("ip4/6_0对应的资源ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_device_id_1", ckdb.UInt32).SetComment("ip4/6_1对应的资源ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_device_type_0", ckdb.UInt8).SetComment("ip4/6_0对应的资源类型"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_device_type_1", ckdb.UInt8).SetComment("ip4/6_1对应的资源类型"))
	}

	if code&L3EpcID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_epc_id", ckdb.Int32).SetComment("ip对应的EPC ID"))
	}
	if code&L3EpcIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_epc_id_0", ckdb.Int32).SetComment("ip4/6_0对应的EPC ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("l3_epc_id_1", ckdb.Int32).SetComment("ip4/6_1对应的EPC ID"))
	}
	if code&L7Protocol != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("l7_protocol", ckdb.UInt8).SetComment("应用协议0: unknown, 1: http, 2: dns, 3: mysql, 4: redis, 5: dubbo, 6: kafka"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("app_service", ckdb.LowCardinalityString))
		columns = append(columns, ckdb.NewColumnWithGroupBy("app_instance", ckdb.LowCardinalityString))
		columns = append(columns, ckdb.NewColumnWithGroupBy("endpoint", ckdb.String))
		columns = append(columns, ckdb.NewColumnWithGroupBy("biz_type", ckdb.UInt8).SetComment("Business Type"))
	}

	if code&MAC != 0 {
		// 不存
		// columns = append(columns, ckdb.NewColumnWithGroupBy("mac", UInt64))
	}
	if code&MACPath != 0 {
		// 不存
		// columns = append(columns, ckdb.NewColumnWithGroupBy("mac_0", UInt64))
		// columns = append(columns, ckdb.NewColumnWithGroupBy("mac_1", UInt64))
	}

	if code&PodClusterID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_cluster_id", ckdb.UInt16).SetComment("ip对应的容器集群ID"))
	}

	if code&PodClusterIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_cluster_id_0", ckdb.UInt16).SetComment("ip4/6_0对应的容器集群ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_cluster_id_1", ckdb.UInt16).SetComment("ip4/6_1对应的容器集群ID"))
	}

	if code&PodGroupID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_group_id", ckdb.UInt32).SetComment("ip对应的容器工作负载ID"))
	}

	if code&PodGroupIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_group_id_0", ckdb.UInt32).SetComment("ip4/6_0对应的容器工作负载ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_group_id_1", ckdb.UInt32).SetComment("ip4/6_1对应的容器工作负载ID"))
	}

	if code&PodID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_id", ckdb.UInt32).SetComment("ip对应的容器POD ID"))
	}

	if code&PodIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_id_0", ckdb.UInt32).SetComment("ip4/6_0对应的容器POD ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_id_1", ckdb.UInt32).SetComment("ip4/6_1对应的容器POD ID"))
	}

	if code&PodNodeID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_node_id", ckdb.UInt32).SetComment("ip对应的容器节点ID"))
	}

	if code&PodNodeIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_node_id_0", ckdb.UInt32).SetComment("ip4/6_0对应的容器节点ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_node_id_1", ckdb.UInt32).SetComment("ip4/6_1对应的容器节点ID"))
	}

	if code&PodNSID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_ns_id", ckdb.UInt16).SetComment("ip对应的容器命名空间ID"))
	}

	if code&PodNSIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_ns_id_0", ckdb.UInt16).SetComment("ip4/6_0对应的容器命名空间ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("pod_ns_id_1", ckdb.UInt16).SetComment("ip4/6_1对应的容器命名空间ID"))
	}

	if code&Protocol != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("protocol", ckdb.UInt8).SetComment("0: 非IP包, 1-255: ip协议号(其中 1:icmp 6:tcp 17:udp)"))
	}

	if code&RegionID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("region_id", ckdb.UInt16).SetComment("ip对应的云平台区域ID"))
	}
	if code&RegionIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("region_id_0", ckdb.UInt16).SetComment("ip4/6_0对应的云平台区域ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("region_id_1", ckdb.UInt16).SetComment("ip4/6_1对应的云平台区域ID"))
	}

	if code&Resource != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_instance_id", ckdb.UInt32).SetComment("ip对应的容器pod优先的资源ID, 取值优先级为pod_id -> pod_node_id -> l3_device_id"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_instance_type", ckdb.UInt8).SetComment("资源类型, 0:IP地址(无法对应资源), 0-100:deviceType(其中10:pod, 14:podNode), 101-200:DeepFlow抽象出的资源(其中101:podGroup, 102:service), 201-255:其他"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_service_id", ckdb.UInt32).SetComment("ip对应的服务优先的资源ID, 取值优先级为service_id  -> pod_node_id -> l3_device_id"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_service_type", ckdb.UInt8).SetComment("资源类型, 0:IP地址(无法对应资源), 0-100:deviceType(其中10:pod, 14:podNode), 101-200:DeepFlow抽象出的资源(其中101:podGroup, 102:service), 201-255:其他"))
	}
	if code&ResourcePath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_instance_id_0", ckdb.UInt32).SetComment("ip0对应的容器pod优先的资源ID, 取值优先级为pod_id -> pod_node_id -> l3_device_id"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_instance_type_0", ckdb.UInt8).SetComment("资源类型, 0:IP地址(无法对应资源), 0-100:deviceType(其中10:pod, 14:podNode), 101-200:DeepFlow抽象出的资源(其中101:podGroup, 102:service), 201-255:其他"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_service_id_0", ckdb.UInt32).SetComment("ip0对应的服务优先的资源ID, 取值优先级为service_id  -> pod_node_id -> l3_device_id"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_service_type_0", ckdb.UInt8).SetComment("资源类型, 0:IP地址(无法对应资源), 0-100:deviceType(其中10:pod, 14:podNode), 101-200:DeepFlow抽象出的资源(其中101:podGroup, 102:service), 201-255:其他"))

		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_instance_id_1", ckdb.UInt32).SetComment("ip1对应的容器pod优先的资源ID, 取值优先级为pod_id -> pod_node_id -> l3_device_id"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_instance_type_1", ckdb.UInt8).SetComment("资源类型, 0:IP地址(无法对应资源), 0-100:deviceType(其中10:pod, 14:podNode), 101-200:DeepFlow抽象出的资源(其中101:podGroup, 102:service), 201-255:其他"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_service_id_1", ckdb.UInt32).SetComment("ip1对应的服务优先的资源ID, 取值优先级为service_id  -> pod_node_id -> l3_device_id"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("auto_service_type_1", ckdb.UInt8).SetComment("资源类型, 0:IP地址(无法对应资源), 0-100:deviceType(其中10:pod, 14:podNode), 101-200:DeepFlow抽象出的资源(其中101:podGroup, 102:service), 201-255:其他"))
	}

	if code&SignalSource != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("signal_source", ckdb.UInt16).SetComment("信号源"))
	}
	if code&ServiceID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("service_id", ckdb.UInt32).SetComment("ip对应的服务ID"))
	}
	if code&ServiceIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("service_id_0", ckdb.UInt32).SetComment("ip4/6_0对应的服务ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("service_id_1", ckdb.UInt32).SetComment("ip4/6_1对应的服务ID"))
	}

	if code&ServerPort != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("server_port", ckdb.UInt16).SetIndex(ckdb.IndexSet).SetComment("服务端端口"))
	}

	if code&SubnetID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("subnet_id", ckdb.UInt16).SetComment("ip对应的子网ID(0: 未找到)"))
	}
	if code&SubnetIDPath != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("subnet_id_0", ckdb.UInt16).SetComment("ip4/6_0对应的子网ID(0: 未找到)"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("subnet_id_1", ckdb.UInt16).SetComment("ip4/6_1对应的子网ID(0: 未找到)"))
	}
	if code&TunnelIPID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("tunnel_ip_id", ckdb.UInt16).SetComment("隧道分发点ID"))
	}
	if code&TAPPort != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("capture_nic_type", ckdb.UInt8).SetIndex(ckdb.IndexNone).SetComment("采集位置标识类型 0: MAC，1: IPv4, 2: IPv6, 3: ID, 4: NetFlow, 5: SFlow"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("tunnel_type", ckdb.UInt8).SetIndex(ckdb.IndexNone).SetComment("隧道封装类型 0：--，1：VXLAN，2：IPIP，3：GRE"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("capture_nic", ckdb.UInt32).SetIndex(ckdb.IndexNone).SetComment("采集位置标识 若capture_network_type_id为3: 虚拟网络流量源, 表示虚拟接口MAC地址低4字节 00000000~FFFFFFFF"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("nat_source", ckdb.UInt8).SetComment("0: NONE, 1: VIP, 2: TOA"))
	}
	if code&TAPSide != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("observation_point", ckdb.LowCardinalityString).SetComment("流量采集位置(c: 客户端(0侧)采集, s: 服务端(1侧)采集)"))
	}
	if code&TAPType != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("capture_network_type_id", ckdb.UInt8).SetComment("流量采集点(1-2,4-255: 接入网络流量, 3: 虚拟网络流量)"))
	}
	if code&VTAPID != 0 {
		columns = append(columns, ckdb.NewColumnWithGroupBy("agent_id", ckdb.UInt16).SetComment("采集器的ID"))
		columns = append(columns, ckdb.NewColumnWithGroupBy("team_id", ckdb.UInt16).SetComment("团队的ID"))
	}

	return columns
}

const TAP_PORT_STR_LEN = 8

func putTAPPort(bs []byte, tapPort uint64) int {
	copy(bs, "00000000")
	s := strconv.FormatUint(tapPort, 16)
	if TAP_PORT_STR_LEN >= len(s) {
		copy(bs[TAP_PORT_STR_LEN-len(s):], s)
	} else {
		copy(bs, s[len(s)-TAP_PORT_STR_LEN:])
	}
	return TAP_PORT_STR_LEN
}

func (t *Tag) String() string {
	var buf strings.Builder
	buf.WriteString("fields:")
	buf.WriteString(t.ToKVString())
	if t.Code&MAC != 0 {
		buf.WriteString(",mac=")
		buf.WriteString(utils.Uint64ToMac(t.MAC).String())
	}
	if t.Code&MACPath != 0 {
		buf.WriteString(",mac_0=")
		buf.WriteString(utils.Uint64ToMac(t.MAC).String())
		buf.WriteString(",mac_1=")
		buf.WriteString(utils.Uint64ToMac(t.MAC1).String())
	}

	buf.WriteString(" code:")
	buf.WriteString(fmt.Sprintf("x%016x", t.Code))
	return buf.String()
}

func (t *Tag) ReadFromPB(p *pb.MiniTag) {
	t.Code = Code(p.Code)
	t.GlobalThreadID = uint8(p.Field.GlobalThreadId)
	t.IsIPv4 = 1 - uint8(p.Field.IsIpv6)
	if t.IsIPv4 == 0 {
		if t.IP6 == nil {
			t.IP6 = make([]byte, 16)
		}
		copy(t.IP6, p.Field.Ip[:net.IPv6len])
		if t.Code&IPPath != 0 {
			if t.IP61 == nil {
				t.IP61 = make([]byte, 16)
			}
			copy(t.IP61, p.Field.Ip1[:net.IPv6len])
		}
	} else {
		t.IP = binary.BigEndian.Uint32(p.Field.Ip[:net.IPv4len])
		if t.Code&IPPath != 0 {
			t.IP1 = binary.BigEndian.Uint32(p.Field.Ip1[:net.IPv4len])
		}
	}
	t.MAC = p.Field.Mac
	t.MAC1 = p.Field.Mac1
	// The range of EPC ID is [-2,65533], if EPC ID < -2 needs to be transformed into the range.
	t.L3EpcID = MarshalInt32WithSpecialID(p.Field.L3EpcId)
	t.L3EpcID1 = MarshalInt32WithSpecialID(p.Field.L3EpcId1)
	direction := DirectionEnum(p.Field.Direction)
	t.Role = direction.ToRole()
	t.TAPSide = TAPSideEnum(p.Field.TapSide)
	t.TAPSideStr = TAPSideEnum(p.Field.TapSide).String()
	t.Protocol = layers.IPProtocol(p.Field.Protocol)
	t.ACLGID = uint16(p.Field.AclGid)
	t.ServerPort = uint16(p.Field.ServerPort)
	t.VTAPID = uint16(p.Field.VtapId)
	t.TAPPort = datatype.TapPort(p.Field.TapPort)
	t.TapPort, t.TapPortType, t.NatSource, t.TunnelType = t.TAPPort.SplitToPortTypeTunnel()
	t.TAPType = TAPTypeEnum(p.Field.TapType)
	t.L7Protocol = datatype.L7Protocol(p.Field.L7Protocol)
	t.AppService = p.Field.AppService
	t.AppInstance = p.Field.AppInstance
	t.Endpoint = p.Field.Endpoint
	t.BizType = uint8(p.Field.BizType)
	// In order to be compatible with the old version of Agent data, GPID needs to be set
	if t.Code&IPPath != 0 {
		t.Code |= GPIDPath
		t.Code |= SignalSource
	} else if t.Code != TRAFFIC_POLICY {
		t.Code |= GPID
		t.Code |= SignalSource
	}
	t.GPID = p.Field.Gpid
	t.GPID1 = p.Field.Gpid1

	if p.Field.PodId != 0 {
		if t.Code&IPPath != 0 && t.Role == ROLE_SERVER {
			t.PodID1 = p.Field.PodId
		} else {
			t.PodID = p.Field.PodId
		}
	}
	t.SignalSource = uint16(p.Field.SignalSource)

	// tunnel_ip_id get from server_port field
	t.TunnelIPID = uint16(p.Field.ServerPort)
}

func (t *Tag) SetID(id string) {
	t.id = id
}

func (t *Tag) GetCode() uint64 {
	return uint64(t.Code)
}

func (t *Tag) SetCode(code uint64) {
	t.Code = Code(code)
}

func (t *Tag) SetTID(tid uint8) {
	t.GlobalThreadID = tid
}

func (t *Tag) GetTAPType() uint8 {
	return uint8(t.TAPType)
}

const (
	SUFFIX_ACL = 1 + iota
	SUFFIX_EDGE
	SUFFIX_ACL_EDGE
	SUFFIX_PORT
	SUFFIX_ACL_PORT
	SUFFIX_EDGE_PORT
	SUFFIX_ACL_EDGE_PORT
)

var DatabaseSuffix = [...]string{
	0:                    "",               // 000
	SUFFIX_ACL:           "_acl",           // 001
	SUFFIX_EDGE:          "_edge",          // 010
	SUFFIX_ACL_EDGE:      "_acl_edge",      // 011
	SUFFIX_PORT:          "_port",          // 100
	SUFFIX_ACL_PORT:      "_acl_port",      // 101
	SUFFIX_EDGE_PORT:     "_edge_port",     // 110
	SUFFIX_ACL_EDGE_PORT: "_acl_edge_port", // 111
}

func (t *Tag) DatabaseSuffixID() int {
	code := 0
	if t.Code&ACLGID != 0 {
		code |= SUFFIX_ACL // 0x1
	}
	if t.Code.HasEdgeTagField() {
		code |= SUFFIX_EDGE // 0x2
	}
	if t.Code&ServerPort != 0 {
		code |= SUFFIX_PORT // 0x4
	}
	return code
}

func (t *Tag) DatabaseSuffix() string {
	return DatabaseSuffix[t.DatabaseSuffixID()]
}

var fieldPool = pool.NewLockFreePool(func() *Field {
	return &Field{}
})

func AcquireField() *Field {
	return fieldPool.Get()
}

func ReleaseField(field *Field) {
	if field == nil {
		return
	}
	*field = Field{}
	fieldPool.Put(field)
}

func CloneField(field *Field) *Field {
	newField := AcquireField()
	*newField = *field
	if field.IP6 != nil {
		newField.IP6 = make(net.IP, len(field.IP6))
		copy(newField.IP6, field.IP6)
	}
	if field.IP61 != nil {
		newField.IP61 = make(net.IP, len(field.IP61))
		copy(newField.IP61, field.IP61)
	}
	return newField
}

var tagPool = pool.NewLockFreePool(func() *Tag {
	return &Tag{}
})

func AcquireTag() *Tag {
	return tagPool.Get()
}

// ReleaseTag 需要释放Tag拥有的Field
func ReleaseTag(tag *Tag) {
	if tag == nil {
		return
	}
	*tag = Tag{}
	tagPool.Put(tag)
}

// CloneTag 需要复制Tag拥有的Field
func CloneTag(tag *Tag) *Tag {
	newTag := AcquireTag()
	newTag.Field = tag.Field
	newTag.Code = tag.Code
	newTag.id = tag.id
	return newTag
}

func (t *Tag) Clone() Tagger {
	return CloneTag(t)
}

func (t *Tag) Release() {
	ReleaseTag(t)
}

func (f *Field) NewTag(c Code) *Tag {
	tag := AcquireTag()
	tag.Field = *f
	tag.Code = c
	tag.id = ""
	return tag
}

func parseUint(s string, base int, bitSize int) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, base, bitSize)
}

// EncodeTagToPromLabels 将 Tag 编码成 prom Label
func EncodeTagToPromLabels(tag *Tag) []prompb.Label {
	if tag == nil {
		return nil
	}
	buffer := make([]byte, MAX_STRING_LENGTH)
	size := tag.MarshalTo(buffer)
	return encodePromLabels(buffer[:size])
}
