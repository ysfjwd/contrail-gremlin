package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/Jeffail/gabs"
	"github.com/go-gremlin/gremlin"
	"github.com/gocql/gocql"
	logging "github.com/op/go-logging"
)

var log = logging.MustGetLogger("gremlin-loader")
var format = logging.MustStringFormatter(
	`%{color}%{time:15:04:05.000} %{shortfunc} ▶ %{level:.4s} %{id:03x}%{color:reset} %{message}`,
)

type Link struct {
	Source string
	Target string
	Type   string
}

func (l Link) Create() error {
	_, err := gremlin.Query("g.V().hasLabel(src).as('src').V().hasLabel(dst).addE(type).from('src')").Bindings(gremlin.Bind{"src": l.Source, "dst": l.Target, "type": l.Type}).Exec()
	return err
}

type Node struct {
	UUID       string
	Properties map[string]interface{}
}

func (n Node) Create() error {
	encoder := GremlinPropertiesEncoder{}
	err := encoder.Encode(n.Properties)
	if err != nil {
		return err
	}
	// drop node first
	gremlin.Query("g.V().hasLabel(uuid).drop()").Bindings(gremlin.Bind{"uuid": n.UUID}).Exec()
	query := fmt.Sprintf("g.addV(\"%s\")%s", n.UUID, encoder.String())
	_, err = gremlin.Query(query).Exec()
	return err
}

func (n Node) AddProperties(prefix string, c *gabs.Container) {
	if _, ok := c.Data().([]interface{}); ok {
		childs, _ := c.Children()
		for _, child := range childs {
			n.AddProperties(prefix, child)
		}
		return
	}
	if _, ok := c.Data().(map[string]interface{}); ok {
		childs, _ := c.ChildrenMap()
		for key, child := range childs {
			n.AddProperties(prefix+"."+key, child)
		}
		return
	}
	if str, ok := c.Data().(string); ok {
		n.AddProperty(prefix, str)
		return
	}
	if num, ok := c.Data().(float64); ok {
		n.AddProperty(prefix, num)
		return
	}
	if boul, ok := c.Data().(bool); ok {
		n.AddProperty(prefix, boul)
		return
	}
	n.AddProperty(prefix, "null")
}

func (n Node) AddProperty(prefix string, value interface{}) {
	if _, ok := n.Properties[prefix]; ok {
		n.Properties[prefix] = append(n.Properties[prefix].([]interface{}), value)
	} else {
		n.Properties[prefix] = []interface{}{value}
	}
}

func main() {

	backend := logging.NewLogBackend(os.Stderr, "", 0)
	backendFormatter := logging.NewBackendFormatter(backend, format)
	logging.SetBackend(backendFormatter)

	if err := gremlin.NewCluster("ws://localhost:8182/gremlin"); err != nil {
		log.Fatal("Failed to connect to gremlin server.")
	} else {
		log.Notice("Connected to Gremlin server.")
	}

	log.Notice("Connecting to Cassandra...")
	cluster := gocql.NewCluster("localhost")
	cluster.Keyspace = "config_db_uuid"
	cluster.Consistency = gocql.Quorum
	session, _ := cluster.CreateSession()
	defer session.Close()
	log.Notice("Connected.")

	var uuid string
	var key string
	var column1 string
	var valueJSON []byte

	var links []Link

	uuids := session.Query(`SELECT DISTINCT key FROM obj_uuid_table`).Iter()
	for uuids.Scan(&uuid) {
		log.Debugf("Processing %s", uuid)
		node := Node{UUID: uuid, Properties: map[string]interface{}{}}
		r := session.Query(`SELECT key, column1, value FROM obj_uuid_table WHERE key=?`, uuid).Iter()
		for r.Scan(&key, &column1, &valueJSON) {
			split := strings.Split(column1, ":")
			switch split[0] {
			case
				"ref",
				"back_ref",
				"children",
				"parent":
				links = append(links, Link{Source: uuid, Target: split[2], Type: split[0]})
			case "prop":
				value, err := gabs.ParseJSON(valueJSON)
				if err != nil {
					log.Fatalf("Failed to parse %v", string(valueJSON))
				}
				node.AddProperties(split[1], value)
			}
		}
		if err := r.Close(); err != nil {
			log.Fatal(err)
		}
		if err := node.Create(); err != nil {
			log.Fatal(err)
		} else {
			log.Debugf("Node %v", node)
		}

	}

	for _, link := range links {
		log.Debugf("Create link %v", link)
		link.Create()
	}

}

type GremlinPropertiesEncoder struct {
	bytes.Buffer
}

func (p *GremlinPropertiesEncoder) EncodeBool(b bool) error {
	if b {
		p.WriteString("true")
	} else {
		p.WriteString("false")
	}
	return nil
}

func (p *GremlinPropertiesEncoder) EncodeString(s string) error {
	p.WriteByte('"')
	p.WriteString(strings.Replace(s, `"`, `\"`, -1))
	p.WriteByte('"')
	return nil
}

func (p *GremlinPropertiesEncoder) EncodeInt64(i int64) error {
	p.WriteString(strconv.FormatInt(i, 10))
	return nil
}

func (p *GremlinPropertiesEncoder) EncodeUint64(i uint64) error {
	p.WriteString(strconv.FormatUint(i, 10))
	return nil
}

func (p *GremlinPropertiesEncoder) EncodeMap(m map[string]interface{}) error {
	for k, v := range m {
		err := p.EncodeKVPair(k, v)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *GremlinPropertiesEncoder) EncodeKVPair(k string, v interface{}) error {
	switch v.(type) {
	case []interface{}:
		for _, i := range v.([]interface{}) {
			p.WriteString(".property(list,")
			p.EncodeString(k)
			p.WriteByte(',')
			err := p.Encode(i)
			if err != nil {
				return err
			}
			p.WriteString(")")
		}
	default:
		p.WriteString(".property(")
		p.EncodeString(k)
		p.WriteByte(',')
		err := p.Encode(v)
		if err != nil {
			return err
		}
		p.WriteString(")")
	}
	return nil
}

func (p *GremlinPropertiesEncoder) Encode(v interface{}) error {
	switch v.(type) {
	case bool:
		return p.EncodeBool(v.(bool))
	case string:
		return p.EncodeString(v.(string))
	case int:
		return p.EncodeInt64(int64(v.(int)))
	case int32:
		return p.EncodeInt64(int64(v.(int32)))
	case int64:
		return p.EncodeInt64(v.(int64))
	case uint:
		return p.EncodeUint64(uint64(v.(uint)))
	case uint32:
		return p.EncodeUint64(uint64(v.(uint32)))
	case uint64:
		return p.EncodeUint64(v.(uint64))
	case float64:
		return p.EncodeInt64(int64(v.(float64)))
	case map[string]interface{}:
		return p.EncodeMap(v.(map[string]interface{}))
	}
	return errors.New("type unsupported: " + reflect.TypeOf(v).String())
}