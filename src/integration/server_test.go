package integration

import (
	"bytes"
	"common"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type ServerSuite struct {
	serverProcesses []*ServerProcess
}

type ServerProcess struct {
	p          *os.Process
	configFile string
	apiPort    int
}

func NewServerProcess(configFile string, apiPort int, d time.Duration, c *C) *ServerProcess {
	s := &ServerProcess{configFile: configFile, apiPort: apiPort}
	err := s.Start()
	c.Assert(err, IsNil)
	if d > 0 {
		time.Sleep(d)
	}
	return s
}

func (self *ServerProcess) Start() error {
	if self.p != nil {
		return fmt.Errorf("Server is already running with pid %d", self.p.Pid)
	}

	dir, err := os.Getwd()
	if err != nil {
		return err
	}

	root := filepath.Join(dir, "..", "..")
	filename := filepath.Join(root, "daemon")
	config := filepath.Join(root, "src/integration/", self.configFile)
	p, err := os.StartProcess(filename, []string{filename, "-config", config}, &os.ProcAttr{
		Dir:   root,
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return err
	}
	self.p = p
	return nil
}

func (self *ServerProcess) Stop() {
	if self.p == nil {
		return
	}

	self.p.Signal(syscall.SIGTERM)
	self.p.Wait()
	self.p = nil
}

func (self *ServerSuite) precreateShards(server *ServerProcess, c *C) {
	time.Sleep(time.Second)
	go self.createShards(server, int64(3600), "false", c)
	go self.createShards(server, int64(86400), "true", c)
	time.Sleep(time.Second)
}

func (self ServerSuite) createShards(server *ServerProcess, bucketSize int64, longTerm string, c *C) {
	serverCount := 3
	nowBucket := time.Now().Unix() / bucketSize * bucketSize
	startIndex := 0

	for i := 0; i <= 50; i++ {
		serverId1 := startIndex%serverCount + 1
		startIndex += 1
		serverId2 := startIndex%serverCount + 1
		startIndex += 1
		data := fmt.Sprintf(`{
			"startTime":%d,
			"endTime":%d,
			"longTerm": %s,
			"shards": [{
				"serverIds": [%d, %d]
			}]
		}`, nowBucket, nowBucket+bucketSize, longTerm, serverId1, serverId2)

		resp := server.Post("/cluster/shards?u=root&p=root", data, c)
		c.Assert(resp.StatusCode, Equals, http.StatusAccepted)
		nowBucket -= bucketSize
	}
}

func ResultsToSeriesCollection(results []*common.SerializedSeries) *SeriesCollection {
	return &SeriesCollection{Members: results}
}

type SeriesCollection struct {
	Members []*common.SerializedSeries
}

func (self *SeriesCollection) GetSeries(name string, c *C) *Series {
	for _, s := range self.Members {
		if s.Name == name {
			return &Series{s}
		}
	}
	c.Fatalf("Couldn't find series '%s' in:\n", name, self)
	return nil
}

type Series struct {
	*common.SerializedSeries
}

func (self *Series) GetValueForPointAndColumn(pointIndex int, columnName string, c *C) interface{} {
	columnIndex := -1
	for index, name := range self.Columns {
		if name == columnName {
			columnIndex = index
		}
	}
	if columnIndex == -1 {
		c.Errorf("Couldn't find column '%s' in series:\n", columnName, self)
		return nil
	}
	if pointIndex > len(self.Points)-1 {
		c.Errorf("Fewer than %d points in series '%s':\n", pointIndex+1, self.Name, self)
	}
	return self.Points[pointIndex][columnIndex]
}

type Point struct {
	Values []interface{}
}

func (self *ServerProcess) Query(database, query string, onlyLocal bool, c *C) *SeriesCollection {
	return self.QueryWithUsername(database, query, onlyLocal, c, "paul", "pass")
}

func (self *ServerProcess) QueryAsRoot(database, query string, onlyLocal bool, c *C) *SeriesCollection {
	return self.QueryWithUsername(database, query, onlyLocal, c, "root", "root")
}

func (self *ServerProcess) QueryWithUsername(database, query string, onlyLocal bool, c *C, username, password string) *SeriesCollection {
	encodedQuery := url.QueryEscape(query)
	fullUrl := fmt.Sprintf("http://localhost:%d/db/%s/series?u=%s&p=%s&q=%s", self.apiPort, database, username, password, encodedQuery)
	if onlyLocal {
		fullUrl = fullUrl + "&force_local=true"
	}
	resp, err := http.Get(fullUrl)
	c.Assert(err, IsNil)
	defer resp.Body.Close()
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	var js []*common.SerializedSeries
	err = json.Unmarshal(body, &js)
	if err != nil {
		fmt.Println("NOT JSON: ", string(body))
	}
	c.Assert(err, IsNil)
	return ResultsToSeriesCollection(js)
}

func (self *ServerProcess) VerifyForbiddenQuery(database, query string, onlyLocal bool, c *C, username, password string) string {
	encodedQuery := url.QueryEscape(query)
	fullUrl := fmt.Sprintf("http://localhost:%d/db/%s/series?u=%s&p=%s&q=%s", self.apiPort, database, username, password, encodedQuery)
	if onlyLocal {
		fullUrl = fullUrl + "&force_local=true"
	}
	resp, err := http.Get(fullUrl)
	c.Assert(err, IsNil)
	defer resp.Body.Close()
	c.Assert(resp.StatusCode, Equals, http.StatusForbidden)
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)

	return string(body)
}

func (self *ServerProcess) Post(url, data string, c *C) *http.Response {
	err := self.Request("POST", url, data, c)
	time.Sleep(time.Millisecond * 10)
	return err
}

func (self *ServerProcess) Delete(url, body string, c *C) *http.Response {
	err := self.Request("DELETE", url, body, c)
	time.Sleep(time.Millisecond * 10)
	return err
}

func (self *ServerProcess) PostGetBody(url, data string, c *C) []byte {
	resp := self.Request("POST", url, data, c)
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	return body
}

func (self *ServerProcess) Get(url string, c *C) []byte {
	resp := self.Request("GET", url, "", c)
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	return body
}

func (self *ServerProcess) Request(method, url, data string, c *C) *http.Response {
	fullUrl := fmt.Sprintf("http://localhost:%d%s", self.apiPort, url)
	req, err := http.NewRequest(method, fullUrl, bytes.NewBufferString(data))
	c.Assert(err, IsNil)
	resp, err := http.DefaultClient.Do(req)
	c.Assert(err, IsNil)
	return resp
}

var _ = Suite(&ServerSuite{})

func (self *ServerSuite) SetUpSuite(c *C) {
	err := os.RemoveAll("/tmp/influxdb/test")
	c.Assert(err, IsNil)
	self.serverProcesses = []*ServerProcess{
		NewServerProcess("test_config1.toml", 60500, time.Second*4, c),
		NewServerProcess("test_config2.toml", 60506, time.Second*4, c),
		NewServerProcess("test_config3.toml", 60510, time.Second*4, c)}
	self.serverProcesses[0].Post("/db?u=root&p=root", "{\"name\":\"full_rep\", \"replicationFactor\":3}", c)
	self.serverProcesses[0].Post("/db?u=root&p=root", "{\"name\":\"test_rep\", \"replicationFactor\":2}", c)
	self.serverProcesses[0].Post("/db?u=root&p=root", "{\"name\":\"single_rep\", \"replicationFactor\":1}", c)
	self.serverProcesses[0].Post("/db?u=root&p=root", "{\"name\":\"test_cq\", \"replicationFactor\":3}", c)
	self.serverProcesses[0].Post("/db/full_rep/users?u=root&p=root", "{\"name\":\"paul\", \"password\":\"pass\", \"isAdmin\": true}", c)
	self.serverProcesses[0].Post("/db/test_rep/users?u=root&p=root", "{\"name\":\"paul\", \"password\":\"pass\", \"isAdmin\": true}", c)
	self.serverProcesses[0].Post("/db/single_rep/users?u=root&p=root", "{\"name\":\"paul\", \"password\":\"pass\", \"isAdmin\": true}", c)
	self.serverProcesses[0].Post("/db/test_cq/users?u=root&p=root", "{\"name\":\"paul\", \"password\":\"pass\", \"isAdmin\": true}", c)
	self.serverProcesses[0].Post("/db/test_cq/users?u=root&p=root", "{\"name\":\"weakpaul\", \"password\":\"pass\", \"isAdmin\": false}", c)
	time.Sleep(time.Second)
	self.precreateShards(self.serverProcesses[0], c)
}

func (self *ServerSuite) TearDownSuite(c *C) {
	for _, s := range self.serverProcesses {
		s.Stop()
	}
}

func (self *ServerSuite) TestWriteAndGetPoint(c *C) {
	data := `
  [{
    "points": [[23.0]],
    "name": "test_write_and_get_point",
    "columns": ["something"]
  }]
  `
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	collection := self.serverProcesses[0].Query("test_rep", "select * from test_write_and_get_point", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_write_and_get_point", c)
	c.Assert(series.Points, HasLen, 1)
	c.Assert(series.GetValueForPointAndColumn(0, "something", c).(float64), Equals, float64(23))
}

func (self *ServerSuite) TestWhereQuery(c *C) {
	data := `[{"points": [[4], [10], [5]], "name": "test_where_query", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	collection := self.serverProcesses[0].Query("test_rep", "select * from test_where_query where value < 6", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_where_query", c)
	c.Assert(series.Points, HasLen, 2)
	c.Assert(series.GetValueForPointAndColumn(0, "value", c).(float64), Equals, float64(5))
	c.Assert(series.GetValueForPointAndColumn(1, "value", c).(float64), Equals, float64(4))
}

func (self *ServerSuite) TestCountQueryOnSingleShard(c *C) {
	data := `[{"points": [[4], [10], [5]], "name": "test_count_query_single_shard", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	t := (time.Now().Unix() - 60) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_count_query_single_shard", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	collection := self.serverProcesses[0].Query("test_rep", "select count(value) from test_count_query_single_shard group by time(1m)", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_count_query_single_shard", c)
	c.Assert(series.Points, HasLen, 2)
	c.Assert(series.GetValueForPointAndColumn(0, "count", c).(float64), Equals, float64(3))
	c.Assert(series.GetValueForPointAndColumn(1, "count", c).(float64), Equals, float64(1))
}

func (self *ServerSuite) TestGroupByDay(c *C) {
	data := `[{"points": [[4], [10], [5]], "name": "test_group_by_day", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	t := (time.Now().Unix() - 86400) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_group_by_day", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	collection := self.serverProcesses[0].Query("test_rep", "select count(value) from test_group_by_day group by time(1d)", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_group_by_day", c)
	c.Assert(series.Points, HasLen, 2)
	c.Assert(series.GetValueForPointAndColumn(0, "count", c).(float64), Equals, float64(3))
	c.Assert(series.GetValueForPointAndColumn(1, "count", c).(float64), Equals, float64(1))
}

func (self *ServerSuite) TestLimitQueryOnSingleShard(c *C) {
	data := `[{"points": [[4], [10], [5]], "name": "test_limit_query_single_shard", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	collection := self.serverProcesses[0].Query("test_rep", "select * from test_limit_query_single_shard limit 2", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_limit_query_single_shard", c)
	c.Assert(series.Points, HasLen, 2)
	c.Assert(series.GetValueForPointAndColumn(0, "value", c).(float64), Equals, float64(5))
	c.Assert(series.GetValueForPointAndColumn(1, "value", c).(float64), Equals, float64(10))
}

func (self *ServerSuite) TestQueryAgainstMultipleShards(c *C) {
	data := `[{"points": [[4], [10], [5]], "name": "test_query_against_multiple_shards", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	t := (time.Now().Unix() - 3600) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_query_against_multiple_shards", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select count(value) from test_query_against_multiple_shards group by time(1h)", false, c)
		c.Assert(collection.Members, HasLen, 1)
		series := collection.GetSeries("test_query_against_multiple_shards", c)
		c.Assert(series.Points, HasLen, 2)
		c.Assert(series.GetValueForPointAndColumn(0, "count", c).(float64), Equals, float64(3))
		c.Assert(series.GetValueForPointAndColumn(1, "count", c).(float64), Equals, float64(1))
	}
}

func (self *ServerSuite) TestQueryAscendingAgainstMultipleShards(c *C) {
	data := `[{"points": [[4], [10]], "name": "test_ascending_against_multiple_shards", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	t := (time.Now().Unix() - 3600) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_ascending_against_multiple_shards", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select * from test_ascending_against_multiple_shards order asc", false, c)
		series := collection.GetSeries("test_ascending_against_multiple_shards", c)
		c.Assert(series.Points, HasLen, 3)
		c.Assert(series.GetValueForPointAndColumn(0, "value", c).(float64), Equals, float64(2))
		c.Assert(series.GetValueForPointAndColumn(1, "value", c).(float64), Equals, float64(4))
		c.Assert(series.GetValueForPointAndColumn(2, "value", c).(float64), Equals, float64(10))
	}
}

func (self *ServerSuite) TestBigGroupByQueryAgainstMultipleShards(c *C) {
	data := `[{"points": [[4], [10]], "name": "test_multiple_shards_big_group_by", "columns": ["value"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	t := (time.Now().Unix() - 3600*2) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_multiple_shards_big_group_by", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	time.Sleep(time.Second)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select count(value) from test_multiple_shards_big_group_by group by time(30d)", false, c)
		series := collection.GetSeries("test_multiple_shards_big_group_by", c)
		c.Assert(series.Points, HasLen, 1)
		c.Assert(series.GetValueForPointAndColumn(0, "count", c).(float64), Equals, float64(3))
	}
}

func (self *ServerSuite) TestWriteSplitToMultipleShards(c *C) {
	data := `[
		{"points": [[4], [10]], "name": "test_write_multiple_shards", "columns": ["value"]},
		{"points": [["asdf"]], "name": "Test_write_multiple_shards", "columns": ["thing"]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select count(value) from test_write_multiple_shards", false, c)
		series := collection.GetSeries("test_write_multiple_shards", c)
		c.Assert(series.Points, HasLen, 1)
		c.Assert(series.GetValueForPointAndColumn(0, "count", c).(float64), Equals, float64(2))

		collection = s.Query("test_rep", "select * from Test_write_multiple_shards", false, c)
		series = collection.GetSeries("Test_write_multiple_shards", c)
		c.Assert(series.Points, HasLen, 1)
		c.Assert(series.GetValueForPointAndColumn(0, "thing", c).(string), Equals, "asdf")
	}
}

func (self *ServerSuite) TestRestartAfterCompaction(c *C) {
	data := `
  [{
    "points": [[1]],
    "name": "test_restart_after_compaction",
    "columns": ["val"]
  }]
  `
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	collection := self.serverProcesses[0].Query("test_rep", "select * from test_restart_after_compaction", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_restart_after_compaction", c)
	c.Assert(series.Points, HasLen, 1)
	resp := self.serverProcesses[0].Post("/raft/force_compaction?u=root&p=root", "", c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	self.serverProcesses[0].Stop()
	time.Sleep(time.Second)
	self.serverProcesses[0].Start()
	time.Sleep(time.Second * 3)

	collection = self.serverProcesses[0].Query("test_rep", "select * from test_restart_after_compaction", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series = collection.GetSeries("test_restart_after_compaction", c)
	c.Assert(series.Points, HasLen, 1)
}

// For issue #140 https://github.com/influxdb/influxdb/issues/140
func (self *ServerSuite) TestRestartServers(c *C) {
	data := `
  [{
    "points": [[1]],
    "name": "test_restart",
    "columns": ["val"]
  }]
  `
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	collection := self.serverProcesses[0].Query("test_rep", "select * from test_restart", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_restart", c)
	c.Assert(series.Points, HasLen, 1)

	for _, s := range self.serverProcesses {
		s.Stop()
	}
	time.Sleep(time.Second)

	err := self.serverProcesses[0].Start()
	c.Assert(err, IsNil)
	time.Sleep(time.Second)
	err = self.serverProcesses[1].Start()
	c.Assert(err, IsNil)
	err = self.serverProcesses[2].Start()
	time.Sleep(time.Second * 5)

	collection = self.serverProcesses[0].Query("test_rep", "select * from test_restart", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series = collection.GetSeries("test_restart", c)
	c.Assert(series.Points, HasLen, 1)
}

func (self *ServerSuite) TestWritingNullInCluster(c *C) {
	data := `[{"name":"test_null_in_cluster","columns":["provider","city"],"points":[["foo", "bar"], [null, "baz"]]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	collection := self.serverProcesses[0].Query("test_rep", "select * from test_null_in_cluster", false, c)
	c.Assert(collection.Members, HasLen, 1)
	c.Assert(collection.GetSeries("test_null_in_cluster", c).Points, HasLen, 2)
}

func (self *ServerSuite) TestCountDistinctWithNullValues(c *C) {
	data := `[{"name":"test_null_with_distinct","columns":["column"],"points":[["value1"], [null], ["value2"], ["value1"], [null]]}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	collection := self.serverProcesses[0].Query("test_rep", "select count(distinct(column)) from test_null_with_distinct group by time(1m)", false, c)
	c.Assert(collection.Members, HasLen, 1)
	series := collection.GetSeries("test_null_with_distinct", c)
	c.Assert(series.GetValueForPointAndColumn(0, "count", c), Equals, float64(2))
}

// issue #147
func (self *ServerSuite) TestExtraSequenceNumberColumns(c *C) {
	data := `
  [{
    "points": [
        ["foo", 1390852524, 1234]
    ],
    "name": "test_extra_sequence_number_column",
    "columns": ["val_1", "time", "sequence_number"]
  }]`
	resp := self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass&time_precision=s", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)

	time.Sleep(time.Second)

	collection := self.serverProcesses[0].Query("test_rep", "select * from test_extra_sequence_number_column", false, c)
	series := collection.GetSeries("test_extra_sequence_number_column", c)
	c.Assert(series.Columns, HasLen, 3)
	c.Assert(series.GetValueForPointAndColumn(0, "sequence_number", c), Equals, float64(1234))
}

// issue #206
func (self *ServerSuite) TestUnicodeSupport(c *C) {
	data := `
  [{
    "points": [
        ["山田太郎", "中文", "⚑"]
    ],
    "name": "test_unicode",
    "columns": ["val_1", "val_2", "val_3"]
  }]`
	resp := self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)

	time.Sleep(time.Second)

	collection := self.serverProcesses[0].Query("test_rep", "select * from test_unicode", false, c)
	series := collection.GetSeries("test_unicode", c)
	c.Assert(series.GetValueForPointAndColumn(0, "val_1", c), Equals, "山田太郎")
	c.Assert(series.GetValueForPointAndColumn(0, "val_2", c), Equals, "中文")
	c.Assert(series.GetValueForPointAndColumn(0, "val_3", c), Equals, "⚑")
}

func (self *ServerSuite) TestSslSupport(c *C) {
	data := `
  [{
    "points": [
        ["val1", 2]
    ],
    "name": "test_sll",
    "columns": ["val_1", "val_2"]
  }]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	encodedQuery := url.QueryEscape("select * from test_sll")
	fullUrl := fmt.Sprintf("https://localhost:60503/db/test_rep/series?u=paul&p=pass&q=%s", encodedQuery)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := http.Client{
		Transport: transport,
	}
	resp, err := client.Get(fullUrl)
	c.Assert(err, IsNil)
	defer resp.Body.Close()
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	body, err := ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	var js []*common.SerializedSeries
	err = json.Unmarshal(body, &js)
	c.Assert(err, IsNil)
	collection := ResultsToSeriesCollection(js)
	series := collection.GetSeries("test_sll", c)
	c.Assert(len(series.Points) > 0, Equals, true)
}

func (self *ServerSuite) TestInvalidUserNameAndDbName(c *C) {
	resp := self.serverProcesses[0].Post("/db/dummy_db/users?u=root&p=3rrpl4n3!", "{\"name\":\"foo%bar\", \"password\":\"root\"}", c)
	c.Assert(resp.StatusCode, Not(Equals), http.StatusOK)
	resp = self.serverProcesses[0].Post("/db/dummy%db/users?u=root&p=3rrpl4n3!", "{\"name\":\"foobar\", \"password\":\"root\"}", c)
	c.Assert(resp.StatusCode, Not(Equals), http.StatusOK)
}

func (self *ServerSuite) TestShouldNotResetRootsPassword(c *C) {
	resp := self.serverProcesses[0].Post("/db/dummy_db/users?u=root&p=root", "{\"name\":\"root\", \"password\":\"pass\"}", c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	time.Sleep(time.Second)
	resp = self.serverProcesses[0].Request("GET", "/db/dummy_db/authenticate?u=root&p=pass", "", c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	resp = self.serverProcesses[0].Request("GET", "/cluster_admins/authenticate?u=root&p=root", "", c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
}

func (self *ServerSuite) TestDeleteReplication(c *C) {
	data := `
  [{
    "points": [
        ["val1", 2]
    ],
    "name": "test_delete_replication",
    "columns": ["val_1", "val_2"]
  }]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	collection := self.serverProcesses[0].Query("test_rep", "select count(val_1) from test_delete_replication", false, c)
	series := collection.GetSeries("test_delete_replication", c)
	c.Assert(series.GetValueForPointAndColumn(0, "count", c), Equals, float64(1))

	for _, s := range self.serverProcesses {
		s.Query("test_rep", "delete from test_delete_replication", false, c)
		collection = self.serverProcesses[0].Query("test_rep", "select count(val_1) from test_delete_replication", false, c)
		c.Assert(collection.Members, HasLen, 0)
	}
}

// Reported by Alex in the following thread
// https://groups.google.com/forum/#!msg/influxdb/I_Ns6xYiMOc/XilTv6BDgHgJ
func (self *ServerSuite) TestAdminPermissionToDeleteData(c *C) {
	data := `
  [{
    "points": [
        ["val1", 2]
    ],
    "name": "test_delete_admin_permission",
    "columns": ["val_1", "val_2"]
  }]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	collection := self.serverProcesses[0].QueryAsRoot("test_rep", "select count(val_1) from test_delete_admin_permission", false, c)
	series := collection.GetSeries("test_delete_admin_permission", c)
	c.Assert(series.GetValueForPointAndColumn(0, "count", c), Equals, float64(1))

	self.serverProcesses[0].Query("test_rep", "delete from test_delete_admin_permission", false, c)
	collection = self.serverProcesses[0].Query("test_rep", "select count(val_1) from test_delete_admin_permission", false, c)
	c.Assert(collection.Members, HasLen, 0)
}

func (self *ServerSuite) TestListSeries(c *C) {
	self.serverProcesses[0].Post("/db?u=root&p=root", `{"name": "list_series", "replicationFactor": 2}`, c)
	self.serverProcesses[0].Post("/db/list_series/users?u=root&p=root", `{"name": "paul", "password": "pass"}`, c)
	time.Sleep(time.Second)
	data := `[{
		"name": "cluster_query",
		"columns": ["val1"],
		"points": [[1]]
		}]`
	self.serverProcesses[0].Post("/db/list_series/series?u=paul&p=pass", data, c)
	t := (time.Now().Unix() - 3600) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "another_query", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/list_series/series?u=paul&p=pass", data, c)

	time.Sleep(time.Second)
	for _, s := range self.serverProcesses {
		collection := s.Query("list_series", "list series", false, c)
		c.Assert(collection.Members, HasLen, 2)
		s := collection.GetSeries("cluster_query", c)
		c.Assert(s, NotNil)
		s = collection.GetSeries("another_query", c)
		c.Assert(s, NotNil)
	}
}

func (self *ServerSuite) TestSelectingTimeColumn(c *C) {
	self.serverProcesses[0].Post("/db?u=root&p=root", `{"name": "test_rep", "replicationFactor": 2}`, c)
	self.serverProcesses[0].Post("/db/test_rep/users?u=root&p=root", `{"name": "paul", "password": "pass"}`, c)
	time.Sleep(time.Second)
	data := `[{
		"name": "selecting_time_column",
		"columns": ["val1"],
		"points": [[1]]
		}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select val1, time from selecting_time_column", false, c)
		s := collection.GetSeries("selecting_time_column", c)
		c.Assert(s.Columns, HasLen, 3)
		c.Assert(s.Points, HasLen, 1)
	}
}

func (self *ServerSuite) TestDropDatabase(c *C) {
	self.serverProcesses[0].Post("/db?u=root&p=root", `{"name": "drop_db", "replicationFactor": 3}`, c)
	self.serverProcesses[0].Post("/db/drop_db/users?u=root&p=root", `{"name": "paul", "password": "pass"}`, c)
	data := `[{
		"name": "cluster_query",
		"columns": ["val1"],
		"points": [[1]]
		}]`
	self.serverProcesses[0].Post("/db/drop_db/series?u=paul&p=pass", data, c)
	time.Sleep(time.Second)
	resp := self.serverProcesses[0].Request("DELETE", "/db/drop_db?u=root&p=root", "", c)
	c.Assert(resp.StatusCode, Equals, http.StatusNoContent)
	time.Sleep(time.Second)
	self.serverProcesses[0].Post("/db?u=root&p=root", `{"name": "drop_db", "replicationFactor": 3}`, c)
	self.serverProcesses[0].Post("/db/drop_db/users?u=root&p=root", `{"name": "paul", "password": "pass"}`, c)
	time.Sleep(time.Second)
	for _, s := range self.serverProcesses {
		fmt.Printf("Running query against: %d\n", s.apiPort)
		collection := s.Query("drop_db", "select * from cluster_query", true, c)
		c.Assert(collection.Members, HasLen, 0)
	}
}

func (self *ServerSuite) TestDropSeries(c *C) {
	for i := 0; i < 2; i++ {
		self.serverProcesses[0].Post("/db?u=root&p=root", `{"name": "drop_series", "replicationFactor": 3}`, c)
		self.serverProcesses[0].Post("/db/drop_series/users?u=root&p=root", `{"name": "paul", "password": "pass"}`, c)
		data := `[{
		"name": "cluster_query",
		"columns": ["val1"],
		"points": [[1]]
		}]`
		self.serverProcesses[0].Post("/db/drop_series/series?u=paul&p=pass", data, c)
		time.Sleep(time.Second)
		if i == 0 {
			fmt.Printf("Using the http api\n")
			resp := self.serverProcesses[0].Request("DELETE", "/db/drop_series/series/cluster_query?u=root&p=root", "", c)
			c.Assert(resp.StatusCode, Equals, http.StatusNoContent)
		} else {
			fmt.Printf("Using the drop series\n")
			self.serverProcesses[0].Query("drop_series", "drop series cluster_query", false, c)
		}
		time.Sleep(time.Second)
		for _, s := range self.serverProcesses {
			fmt.Printf("Running query against: %d\n", s.apiPort)
			collection := s.Query("drop_series", "select * from cluster_query", true, c)
			c.Assert(collection.Members, HasLen, 0)
		}
	}
}

func (self *ServerSuite) TestFailureAndReplicationReplays(c *C) {
	// write data and confirm that it went to all three servers
	data := `
  [{
    "points": [
        [1]
    ],
    "name": "test_failure_replays",
    "columns": ["val"]
  }]`
	self.serverProcesses[0].Post("/db/full_rep/series?u=paul&p=pass", data, c)

	time.Sleep(time.Second) // wait for data to get replicated

	for _, s := range self.serverProcesses {
		collection := s.Query("full_rep", "select sum(val) from test_failure_replays;", true, c)
		series := collection.GetSeries("test_failure_replays", c)
		c.Assert(series.Points, HasLen, 1)
	}

	self.serverProcesses[1].Stop()
	time.Sleep(time.Second)
	data = `
	[{
		"points": [[2]],
		"name": "test_failure_replays",
		"columns": ["val"]
	}]
	`
	self.serverProcesses[0].Post("/db/full_rep/series?u=paul&p=pass", data, c)

	expected := []float64{float64(3), 0, float64(3)}
	for i, s := range self.serverProcesses {
		if i == 1 {
			continue
		}

		collection := s.Query("full_rep", "select sum(val) from test_failure_replays;", true, c)
		series := collection.GetSeries("test_failure_replays", c)
		c.Assert(series.GetValueForPointAndColumn(0, "sum", c), Equals, expected[i])
	}

	self.serverProcesses[1].Start()

	for i := 0; i < 3; i++ {
		// wait for the server to startup and the WAL to be synced
		time.Sleep(2 * time.Second)
		collection := self.serverProcesses[1].Query("full_rep", "select sum(val) from test_failure_replays;", true, c)
		series := collection.GetSeries("test_failure_replays", c)
		if series.GetValueForPointAndColumn(0, "sum", c).(float64) == 3 {
			return
		}
	}
	c.Error("write didn't replay properly")
}

// func (self *ServerSuite) TestFailureAndDeleteReplays(c *C) {
// 	data := `
//   [{
//     "points": [
//         [1]
//     ],
//     "name": "test_failure_delete_replays",
//     "columns": ["val"]
//   }]`
// 	self.serverProcesses[0].Post("/db/full_rep/series?u=paul&p=pass", data, c)
// 	fmt.Println("TEST: posted failure")
// 	for _, s := range self.serverProcesses {
// 		collection := s.Query("full_rep", "select val from test_failure_delete_replays", true, c)
// 		series := collection.GetSeries("test_failure_delete_replays", c)
// 		c.Assert(series.Points, HasLen, 1)
// 	}
// 	fmt.Println("TEST: did queries, now stopping")
// 	self.serverProcesses[1].Stop()
// 	fmt.Println("TEST: running delete query on server 0")
// 	self.serverProcesses[0].Query("full_rep", "delete from test_failure_delete_replays", false, c)
// 	fmt.Println("TEST: done!")
// 	time.Sleep(time.Second)
// 	for i, s := range self.serverProcesses {
// 		if i == 1 {
// 			continue
// 		} else {
// 			fmt.Println("TEST: running sum query on server ", i)
// 			collection := s.Query("full_rep", "select sum(val) from test_failure_delete_replays;", true, c)

// 			c.Assert(collection.Members, HasLen, 0)
// 		}
// 	}

// 	fmt.Println("TEST: starting server")
// 	self.serverProcesses[1].Start()
// 	time.Sleep(2 * time.Second)
// 	sum := 0
// 	for i := 0; i < 3; i++ {
// 		fmt.Println("TEST: running sum query on server")
// 		collection := self.serverProcesses[1].Query("full_rep", "select sum(val) from test_failure_delete_replays;", true, c)
// 		sum += len(collection.Members)
// 	}

// 	c.Assert(sum, Equals, 0)
// }

// For issue #130 https://github.com/influxdb/influxdb/issues/130
func (self *ServerSuite) TestColumnNamesReturnInDistributedQuery(c *C) {
	data := `[{
		"name": "cluster_query_with_columns",
		"columns": ["col1"],
		"points": [[1], [2]]
		}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select * from cluster_query_with_columns", false, c)
		series := collection.GetSeries("cluster_query_with_columns", c)
		set := map[float64]bool{}
		for idx, _ := range series.Points {
			set[series.GetValueForPointAndColumn(idx, "col1", c).(float64)] = true
		}
		c.Assert(set, DeepEquals, map[float64]bool{1: true, 2: true})
	}
}

func generateHttpApiSeries(name string, n int) *common.SerializedSeries {
	points := [][]interface{}{}

	for i := 0; i < n; i++ {
		points = append(points, []interface{}{i})
	}

	return &common.SerializedSeries{
		Name:    name,
		Columns: []string{"value"},
		Points:  points,
	}
}

func (self *ServerSuite) TestLimitWithRegex(c *C) {
	// run the test once with less than POINT_BATCH_SIZE points and once
	// with more than POINT_BATCH_SIZE points
	for _, numberOfPoints := range []int{100, 1000} {
		for i := 0; i < 100; i++ {
			name := fmt.Sprintf("limit_with_regex_%d_%d", numberOfPoints, i)
			series := generateHttpApiSeries(name, numberOfPoints)
			bytes, err := json.Marshal([]*common.SerializedSeries{series})
			c.Assert(err, IsNil)
			resp := self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", string(bytes), c)
			defer resp.Body.Close()
			c.Assert(resp.StatusCode, Equals, http.StatusOK)
		}
		time.Sleep(time.Second * 2)
		query := fmt.Sprintf("select * from /.*limit_with_regex_%d.*/ limit 1", numberOfPoints)
		collection := self.serverProcesses[0].Query("test_rep", query, false, c)
		// make sure all series get back 1 point only
		for i := 0; i < 100; i++ {
			table := fmt.Sprintf("limit_with_regex_%d_%d", numberOfPoints, i)
			series := collection.GetSeries(table, c)
			c.Assert(series.SerializedSeries.Points, HasLen, 1)
			c.Assert(series.GetValueForPointAndColumn(0, "value", c), Equals, float64(numberOfPoints-1))
		}
	}
}

// For issue #131 https://github.com/influxdb/influxdb/issues/131
func (self *ServerSuite) TestSelectFromRegexInCluster(c *C) {
	data := `[{
		"name": "cluster_regex_query",
		"columns": ["col1", "col2"],
		"points": [[1, "foo"], [23, "bar"]]
		},{
			"name": "cluster_regex_query_number2",
			"columns": ["blah"],
			"points": [[true]]
		},{
			"name": "Cluster_regex_query_3",
			"columns": ["foobar"],
			"points": [["asdf"]]
			}]`
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)
	time.Sleep(time.Second * 2)
	for _, s := range self.serverProcesses {
		collection := s.Query("test_rep", "select * from /.*/ limit 1", false, c)
		series := collection.GetSeries("cluster_regex_query", c)
		c.Assert(series.GetValueForPointAndColumn(0, "col1", c), Equals, float64(23))
		c.Assert(series.GetValueForPointAndColumn(0, "col2", c), Equals, "bar")
		series = collection.GetSeries("cluster_regex_query_number2", c)
		c.Assert(series.GetValueForPointAndColumn(0, "blah", c), Equals, true)
		series = collection.GetSeries("Cluster_regex_query_3", c)
		c.Assert(series.GetValueForPointAndColumn(0, "foobar", c), Equals, "asdf")
	}
}

func (self *ServerSuite) TestContinuousQueryManagement(c *C) {
	collection := self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series := collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 0)

	response := self.serverProcesses[0].VerifyForbiddenQuery("test_cq", "select * from foo into bar;", false, c, "weakpaul", "pass")
	c.Assert(response, Equals, "Insufficient permissions to create continuous query")

	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from foo into bar;", false, c)

	collection = self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series = collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 1)
	c.Assert(series.GetValueForPointAndColumn(0, "id", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(0, "query", c), Equals, "select * from foo into bar;")

	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from quu into qux;", false, c)
	collection = self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series = collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 2)
	c.Assert(series.GetValueForPointAndColumn(0, "id", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(0, "query", c), Equals, "select * from foo into bar;")
	c.Assert(series.GetValueForPointAndColumn(1, "id", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(1, "query", c), Equals, "select * from quu into qux;")

	self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 1;", false, c)

	collection = self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series = collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 1)
	c.Assert(series.GetValueForPointAndColumn(0, "id", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(0, "query", c), Equals, "select * from quu into qux;")

	self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 2;", false, c)
}

func (self *ServerSuite) TestContinuousQueryFanoutOperations(c *C) {
	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from s1 into d1;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from s2 into d2;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from /s\\d/ into d3;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from silly_name into :series_name.foo;", false, c)
	defer self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 1;", false, c)
	defer self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 2;", false, c)
	defer self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 3;", false, c)
	defer self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 4;", false, c)
	collection := self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series := collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 4)

	data := `[
    {"name": "s1", "columns": ["c1", "c2"], "points": [[1, "a"], [2, "b"]]},
    {"name": "s2", "columns": ["c3"], "points": [[3]]},
    {"name": "silly_name", "columns": ["c4", "c5"], "points": [[4,5]]}
  ]`

	self.serverProcesses[0].Post("/db/test_cq/series?u=paul&p=pass", data, c)

	collection = self.serverProcesses[0].Query("test_cq", "select * from s1;", false, c)
	series = collection.GetSeries("s1", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c1", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(0, "c2", c), Equals, "b")
	c.Assert(series.GetValueForPointAndColumn(1, "c1", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(1, "c2", c), Equals, "a")

	collection = self.serverProcesses[0].Query("test_cq", "select * from s2;", false, c)
	series = collection.GetSeries("s2", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c3", c), Equals, float64(3))

	collection = self.serverProcesses[0].Query("test_cq", "select * from d1;", false, c)
	series = collection.GetSeries("d1", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c1", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(0, "c2", c), Equals, "b")
	c.Assert(series.GetValueForPointAndColumn(1, "c1", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(1, "c2", c), Equals, "a")

	collection = self.serverProcesses[0].Query("test_cq", "select * from d2;", false, c)
	series = collection.GetSeries("d2", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c3", c), Equals, float64(3))

	collection = self.serverProcesses[0].Query("test_cq", "select * from d3;", false, c)
	series = collection.GetSeries("d3", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c3", c), Equals, float64(3))
	c.Assert(series.GetValueForPointAndColumn(1, "c1", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(1, "c2", c), Equals, "b")
	c.Assert(series.GetValueForPointAndColumn(2, "c1", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(2, "c2", c), Equals, "a")

	collection = self.serverProcesses[0].Query("test_cq", "select * from silly_name.foo;", false, c)
	series = collection.GetSeries("silly_name.foo", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c4", c), Equals, float64(4))
	c.Assert(series.GetValueForPointAndColumn(0, "c5", c), Equals, float64(5))
}

func (self *ServerSuite) TestContinuousQueryGroupByOperations(c *C) {
	currentTime := time.Now()

	previousTime := currentTime.Truncate(10 * time.Second)
	oldTime := time.Unix(previousTime.Unix()-5, 0).Unix()
	oldOldTime := time.Unix(previousTime.Unix()-10, 0).Unix()

	data := fmt.Sprintf(`[
    {"name": "s3", "columns": ["c1", "c2", "time"], "points": [
      [1, "a", %d],
      [2, "b", %d],
      [3, "c", %d],
      [7, "x", %d],
      [8, "y", %d],
      [9, "z", %d]
    ]}
  ]`, oldTime, oldTime, oldTime, oldOldTime, oldOldTime, oldOldTime)

	self.serverProcesses[0].Post("/db/test_cq/series?u=paul&p=pass&time_precision=s", data, c)

	time.Sleep(time.Second)

	self.serverProcesses[0].QueryAsRoot("test_cq", "select mean(c1) from s3 group by time(5s) into d3.mean;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "select count(c2) from s3 group by time(5s) into d3.count;", false, c)

	collection := self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series := collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 2)

	time.Sleep(2 * time.Second)

	collection = self.serverProcesses[0].Query("test_cq", "select * from s3;", false, c)
	series = collection.GetSeries("s3", c)
	c.Assert(series.Points, HasLen, 6)

	collection = self.serverProcesses[0].Query("test_cq", "select * from d3.mean;", false, c)
	series = collection.GetSeries("d3.mean", c)
	c.Assert(series.GetValueForPointAndColumn(0, "mean", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(1, "mean", c), Equals, float64(8))

	collection = self.serverProcesses[0].Query("test_cq", "select * from d3.count;", false, c)
	series = collection.GetSeries("d3.count", c)
	c.Assert(series.GetValueForPointAndColumn(0, "count", c), Equals, float64(3))
	c.Assert(series.GetValueForPointAndColumn(1, "count", c), Equals, float64(3))

	self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 1;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 2;", false, c)
}

func (self *ServerSuite) TestContinuousQueryInterpolation(c *C) {
	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from s1 into :series_name.foo;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "select * from s2 into :series_name.foo.[c3];", false, c)
	/* self.serverProcesses[0].QueryAsRoot("test_cq", "select average(c4), count(c5) from s3 group by time(1h) into [average].[count];", false, c) */

	data := `[
    {"name": "s1", "columns": ["c1", "c2"], "points": [[1, "a"], [2, "b"]]},
    {"name": "s2", "columns": ["c3"], "points": [[3]]},
    {"name": "s3", "columns": ["c4", "c5"], "points": [[4,5], [5,6], [6,7]]}
  ]`

	self.serverProcesses[0].Post("/db/test_cq/series?u=paul&p=pass", data, c)

	collection := self.serverProcesses[0].QueryAsRoot("test_cq", "list continuous queries;", false, c)
	series := collection.GetSeries("continuous queries", c)
	c.Assert(series.Points, HasLen, 2)

	collection = self.serverProcesses[0].Query("test_cq", "select * from s1;", false, c)
	series = collection.GetSeries("s1", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c1", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(0, "c2", c), Equals, "b")
	c.Assert(series.GetValueForPointAndColumn(1, "c1", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(1, "c2", c), Equals, "a")

	collection = self.serverProcesses[0].Query("test_cq", "select * from s2;", false, c)
	series = collection.GetSeries("s2", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c3", c), Equals, float64(3))

	collection = self.serverProcesses[0].Query("test_cq", "select * from s1.foo;", false, c)
	series = collection.GetSeries("s1.foo", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c1", c), Equals, float64(2))
	c.Assert(series.GetValueForPointAndColumn(0, "c2", c), Equals, "b")
	c.Assert(series.GetValueForPointAndColumn(1, "c1", c), Equals, float64(1))
	c.Assert(series.GetValueForPointAndColumn(1, "c2", c), Equals, "a")

	collection = self.serverProcesses[0].Query("test_cq", "select * from s2.foo.3;", false, c)
	series = collection.GetSeries("s2.foo.3", c)
	c.Assert(series.GetValueForPointAndColumn(0, "c3", c), Equals, float64(3))

	self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 1;", false, c)
	self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 2;", false, c)
	/* self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 3;", false, c) */
}

func (self *ServerSuite) TestGetServers(c *C) {
	body := self.serverProcesses[0].Get("/cluster/servers?u=root&p=root", c)

	res := make([]interface{}, 0)
	err := json.Unmarshal(body, &res)
	c.Assert(err, IsNil)
	for _, js := range res {
		server := js.(map[string]interface{})
		c.Assert(server["id"], NotNil)
		c.Assert(server["protobufConnectString"], NotNil)
	}
}

func (self *ServerSuite) TestCreateAndGetShards(c *C) {
	// put this far in the future so it doesn't mess up the other tests
	secondsOffset := int64(86400 * 365)
	startSeconds := time.Now().Unix() + secondsOffset
	endSeconds := startSeconds + 3600
	data := fmt.Sprintf(`{
		"startTime":%d,
		"endTime":%d,
		"longTerm": false,
		"shards": [{
			"serverIds": [%d, %d]
		}]
	}`, startSeconds, endSeconds, 2, 3)
	resp := self.serverProcesses[0].Post("/cluster/shards?u=root&p=root", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusAccepted)
	time.Sleep(time.Second)
	for _, s := range self.serverProcesses {
		body := s.Get("/cluster/shards?u=root&p=root", c)
		res := make(map[string]interface{})
		err := json.Unmarshal(body, &res)
		c.Assert(err, IsNil)
		hasShard := false
		for _, s := range res["shortTerm"].([]interface{}) {
			sh := s.(map[string]interface{})
			if sh["startTime"].(float64) == float64(startSeconds) && sh["endTime"].(float64) == float64(endSeconds) {
				servers := sh["serverIds"].([]interface{})
				c.Assert(servers, HasLen, 2)
				c.Assert(servers[0].(float64), Equals, float64(2))
				c.Assert(servers[1].(float64), Equals, float64(3))
				hasShard = true
				break
			}
		}
		c.Assert(hasShard, Equals, true)
	}

}

func (self *ServerSuite) TestDropShard(c *C) {
	// put this far in the future so it doesn't mess up the other tests
	secondsOffset := int64(86400 * 720)
	startSeconds := time.Now().Unix() + secondsOffset
	endSeconds := startSeconds + 3600
	data := fmt.Sprintf(`{
		"startTime":%d,
		"endTime":%d,
		"longTerm": false,
		"shards": [{
			"serverIds": [%d, %d]
		}]
	}`, startSeconds, endSeconds, 1, 2)
	resp := self.serverProcesses[0].Post("/cluster/shards?u=root&p=root", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusAccepted)

	// now write some data to ensure that the local files get created
	t := (time.Now().Unix() + secondsOffset) * 1000
	data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_drop_shard", "columns": ["value", "time"]}]`, t)
	self.serverProcesses[0].Post("/db/test_rep/series?u=paul&p=pass", data, c)

	// and find the shard id
	body := self.serverProcesses[0].Get("/cluster/shards?u=root&p=root", c)
	res := make(map[string]interface{})
	err := json.Unmarshal(body, &res)
	c.Assert(err, IsNil)
	var shardId int
	for _, s := range res["shortTerm"].([]interface{}) {
		sh := s.(map[string]interface{})
		if sh["startTime"].(float64) == float64(startSeconds) && sh["endTime"].(float64) == float64(endSeconds) {
			shardId = int(sh["id"].(float64))
			break
		}
	}

	// confirm the shard files are there
	shardDirServer1 := fmt.Sprintf("/tmp/influxdb/test/1/db/shard_db/%.5d", shardId)
	shardDirServer2 := fmt.Sprintf("/tmp/influxdb/test/2/db/shard_db/%.5d", shardId)
	exists, _ := dirExists(shardDirServer1)
	c.Assert(exists, Equals, true)
	exists, _ = dirExists(shardDirServer2)
	c.Assert(exists, Equals, true)

	// now drop and confirm they're gone
	data = fmt.Sprintf(`{"serverIds": [%d]}`, 1)

	resp = self.serverProcesses[0].Delete(fmt.Sprintf("/cluster/shards/%d?u=root&p=root", shardId), data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusAccepted)
	time.Sleep(time.Second)

	for _, s := range self.serverProcesses {
		body := s.Get("/cluster/shards?u=root&p=root", c)
		res := make(map[string]interface{})
		err := json.Unmarshal(body, &res)
		c.Assert(err, IsNil)
		hasShard := false
		for _, s := range res["shortTerm"].([]interface{}) {
			sh := s.(map[string]interface{})
			if int(sh["id"].(float64)) == shardId {
				hasShard = true
				hasServer := false
				for _, serverId := range sh["serverIds"].([]interface{}) {
					if serverId.(float64) == float64(1) {
						hasServer = true
						break
					}
				}
				c.Assert(hasServer, Equals, false)
			}
		}
		c.Assert(hasShard, Equals, true)
	}

	exists, _ = dirExists(shardDirServer1)
	c.Assert(exists, Equals, false)
	exists, _ = dirExists(shardDirServer2)
	c.Assert(exists, Equals, true)

	// now drop the shard from the last server and confirm it's gone
	data = fmt.Sprintf(`{"serverIds": [%d]}`, 2)

	resp = self.serverProcesses[0].Delete(fmt.Sprintf("/cluster/shards/%d?u=root&p=root", shardId), data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusAccepted)
	time.Sleep(time.Second)

	for _, s := range self.serverProcesses {
		body := s.Get("/cluster/shards?u=root&p=root", c)
		res := make(map[string]interface{})
		err := json.Unmarshal(body, &res)
		c.Assert(err, IsNil)
		hasShard := false
		for _, s := range res["shortTerm"].([]interface{}) {
			sh := s.(map[string]interface{})
			if int(sh["id"].(float64)) == shardId {
				hasShard = true
			}
		}
		c.Assert(hasShard, Equals, false)
	}

	exists, _ = dirExists(shardDirServer1)
	c.Assert(exists, Equals, false)
	exists, _ = dirExists(shardDirServer2)
	c.Assert(exists, Equals, false)
}

func dirExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (self *ServerSuite) TestContinuousQueryWithMixedGroupByOperations(c *C) {
	data := fmt.Sprintf(`[
  {
    "name": "cqtest",
    "columns": ["time", "reqtime", "url"],
    "points": [
        [0, 8.0, "/login"],
        [0, 3.0, "/list"],
        [0, 4.0, "/register"],
        [5, 9.0, "/login"],
        [5, 4.0, "/list"],
        [5, 5.0, "/register"]
    ]
  }
  ]`)

	self.serverProcesses[0].Post("/db/test_cq/series?u=paul&p=pass&time_precision=s", data, c)

	time.Sleep(time.Second)

	self.serverProcesses[0].QueryAsRoot("test_cq", "select mean(reqtime), url from cqtest group by time(10s), url into cqtest.10s", false, c)
	defer self.serverProcesses[0].QueryAsRoot("test_cq", "drop continuous query 1;", false, c)

	collection := self.serverProcesses[0].QueryAsRoot("test_cq", "select * from cqtest.10s", false, c)
	series := collection.GetSeries("cqtest.10s", c)

	c.Assert(series.GetValueForPointAndColumn(0, "mean", c), Equals, float64(8.5))
	c.Assert(series.GetValueForPointAndColumn(0, "url", c), Equals, "/login")
	c.Assert(series.GetValueForPointAndColumn(1, "mean", c), Equals, float64(3.5))
	c.Assert(series.GetValueForPointAndColumn(1, "url", c), Equals, "/list")
	c.Assert(series.GetValueForPointAndColumn(2, "mean", c), Equals, float64(4.5))
	c.Assert(series.GetValueForPointAndColumn(2, "url", c), Equals, "/register")
}
