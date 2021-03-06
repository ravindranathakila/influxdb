package integration

import (
	"engine"
	"fmt"
	"reflect"
	"strings"
	"time"

	. "integration/helpers"

	"net/http"

	influxdb "github.com/influxdb/influxdb-go"
	. "launchpad.net/gocheck"
)

type DataTestSuite struct {
	server *Server
}

var _ = Suite(&DataTestSuite{})

func (self *DataTestSuite) SetUpSuite(c *C) {
	self.server = NewServer("src/integration/test_config_single.toml", c)
}

func (self *DataTestSuite) TearDownSuite(c *C) {
	if self.server == nil {
		return
	}
	self.server.Stop()
}

func (self *DataTestSuite) TestAll(c *C) {
	t := reflect.TypeOf(self)
	v := reflect.ValueOf(self)

	names := []string{}
	setup := []Fun{}
	test := []Fun{}

	client := &DataTestClient{}

	for i := 0; i < t.NumMethod(); i++ {
		method := t.Method(i)
		// the first argument is the DataTestSuite pointer
		if method.Type.NumIn() != 2 || method.Type.NumOut() != 2 {
			continue
		}
		if method.Type.In(1) != reflect.TypeOf(c) {
			c.Logf("Skipping1 %s", method.Name)
			continue
		}

		var fun Fun
		if method.Type.Out(0) != reflect.TypeOf(fun) || method.Type.Out(1) != reflect.TypeOf(fun) {
			c.Logf("Skipping2 %s", method.Name)
			continue
		}
		m := v.MethodByName(method.Name)
		returnValues := m.Call([]reflect.Value{reflect.ValueOf(c)})
		setup = append(setup, returnValues[0].Interface().(Fun))
		test = append(test, returnValues[1].Interface().(Fun))
		names = append(names, method.Name)
	}

	c.Logf("Running %d data tests", len(names))

	for idx, _ := range setup {
		c.Logf("Initializing database for %s", names[idx])
		client.CreateDatabase(fmt.Sprintf("db%d", idx), c)
	}

	self.server.WaitForServerToSync()

	for idx, s := range setup {
		client.SetDB(fmt.Sprintf("db%d", idx))
		c.Logf("Writing data for %s", names[idx])
		s(client)
	}

	self.server.WaitForServerToSync()

	// make sure the tests don't use an idle connection, otherwise the
	// server will close it
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()

	for idx, t := range test {
		client.SetDB(fmt.Sprintf("db%d", idx))
		c.Logf("Started %s", names[idx])
		t(client)
		c.Logf("Finished %s", names[idx])
	}
}

type Fun func(client Client)

// issue #512
func (self *DataTestSuite) GroupByNullValues(c *C) (Fun, Fun) {
	// make sure we exceed the pointBatchSize, so we force a yield to
	// the filtering engine
	return func(client Client) {
			data := `
[
  {
    "points": [
        ["one", null],
        ["one", 1],
        ["one", null]
    ],
    "name": "test_null_groups",
    "columns": ["column0", "column1"]
  }
]`
			client.WriteJsonData(data, c, influxdb.Second)
		}, func(client Client) {
			serieses := client.RunQuery("select count(column0) from test_null_groups group by column1", c, "m")
			c.Assert(serieses, HasLen, 1)
			maps := ToMap(serieses[0])
			c.Assert(maps, HasLen, 2)
			c.Assert(maps[0]["count"], Equals, 1.0)
			// this is an implementation detail, but nulls come last in the
			// trie
			c.Assert(maps[1]["count"], Equals, 2.0)
		}
}

// issue #389
func (self *DataTestSuite) FilteringShouldNotStopIfAllPointsDontMatch(c *C) (Fun, Fun) {
	// make sure we exceed the pointBatchSize, so we force a yield to
	// the filtering engine
	return func(client Client) {
			numberOfPoints := 1000000
			serieses := CreatePointsFromFunc("test_filtering_shouldnt_stop", 1, numberOfPoints, func(i int) float64 { return float64(numberOfPoints - i) })
			series := serieses[0]
			series.Columns = append(series.Columns, "time")
			points := series.Points
			series.Points = nil
			now := time.Now()
			for idx, point := range points {
				point = append(point, float64(now.Add(time.Duration(-idx)*time.Second).Unix()))
				series.Points = append(series.Points, point)
			}
			client.WriteData([]*influxdb.Series{series}, c, influxdb.Second)
		}, func(client Client) {
			serieses := client.RunQuery("select column0 from test_filtering_shouldnt_stop where column0 < 10", c, "m")
			fmt.Printf("serieses: %#v\n", serieses)
			c.Assert(serieses, HasLen, 1)
		}
}

// issue #413
func (self *DataTestSuite) SmallGroupByIntervals(c *C) (Fun, Fun) {
	return func(client Client) {
			serieses := CreatePoints("test_small_group_by", 1, 1)
			client.WriteData(serieses, c)
		}, func(client Client) {
			serieses := client.RunQuery("select count(column0) from test_small_group_by group by time(10)", c, "m")
			c.Assert(serieses, HasLen, 1)
			c.Assert(serieses[0].Points, HasLen, 1)
			c.Assert(serieses[0].Points[0], HasLen, 2)
			c.Assert(serieses[0].Points[0][1], Equals, 1.0)
		}
}

// issue #392
func (self *DataTestSuite) DifferentColumnsAcrossShards(c *C) (Fun, Fun) {
	return func(client Client) {
			i := 0.0
			serieses := CreatePointsFromFunc("test_different_columns_across_shards", 1, 1, func(_ int) float64 { i++; return i })
			now := time.Now().Truncate(24 * time.Hour)
			serieses[0].Columns = []string{"column0", "time"}
			serieses[0].Points[0] = append(serieses[0].Points[0], now.Unix())
			client.WriteData(serieses, c, "s")
			serieses = CreatePointsFromFunc("test_different_columns_across_shards", 2, 1, func(_ int) float64 { i++; return i })
			serieses[0].Columns = []string{"column0", "column1", "time"}
			serieses[0].Points[0] = append(serieses[0].Points[0], now.Add(-8*24*time.Hour).Unix())
			client.WriteData(serieses, c, "s")
		}, func(client Client) {
			serieses := client.RunQuery("select * from test_different_columns_across_shards", c, "s")
			c.Assert(serieses, HasLen, 1)

			maps := ToMap(serieses[0])
			c.Assert(maps[0]["column0"], Equals, 1.0)
			c.Assert(maps[0]["column1"], IsNil)
			c.Assert(maps[1]["column0"], Equals, 2.0)
			c.Assert(maps[1]["column1"], Equals, 3.0)
		}
}

// issue #392
func (self *DataTestSuite) DifferentColumnsAcrossShards2(c *C) (Fun, Fun) {
	return func(client Client) {
			i := 0.0
			serieses := CreatePointsFromFunc("test_different_columns_across_shards_2", 1, 1, func(_ int) float64 { i++; return i })
			now := time.Now().Truncate(24 * time.Hour)
			serieses[0].Columns = []string{"column1", "time"}
			serieses[0].Points[0] = append(serieses[0].Points[0], now.Add(-13*24*time.Hour).Unix())
			client.WriteData(serieses, c, "s")
			serieses = CreatePointsFromFunc("test_different_columns_across_shards_2", 2, 1, func(_ int) float64 { i++; return i })
			serieses[0].Columns = []string{"column1", "column0", "time"}
			serieses[0].Points[0] = append(serieses[0].Points[0], now.Unix())
			client.WriteData(serieses, c, "s")
		}, func(client Client) {
			serieses := client.RunQuery("select * from test_different_columns_across_shards_2", c, "s")
			c.Assert(serieses, HasLen, 1)

			maps := ToMap(serieses[0])
			c.Assert(maps[0]["column0"], Equals, 3.0)
			c.Assert(maps[0]["column1"], Equals, 2.0)
			c.Assert(maps[1]["column0"], IsNil)
			c.Assert(maps[1]["column1"], Equals, 1.0)
		}
}

// issue #455
func (self *DataTestSuite) NullValuesInComparison(c *C) (Fun, Fun) {
	return func(client Client) {
			series := []*influxdb.Series{
				&influxdb.Series{
					Name:    "foo",
					Columns: []string{"foo", "bar"},
					Points: [][]interface{}{
						[]interface{}{1, 2},
						[]interface{}{2, nil},
					},
				},
			}
			client.WriteData(series, c, "s")
		}, func(client Client) {
			serieses := client.RunQuery("select * from foo where bar < 10", c, "s")
			c.Assert(serieses, HasLen, 1)
			maps := ToMap(serieses[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["bar"], Equals, 2.0)
		}
}

func (self *DataTestSuite) ExplainsWithPassthrough(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["val1", 2],
        ["val1", 3]
    ],
    "name": "test_explain_passthrough",
    "columns": ["val_1", "val_2"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("explain select val_1 from test_explain_passthrough where time > now() - 1h", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "explain query")
			c.Assert(series[0].Columns, HasLen, 7) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 1)
			c.Assert(series[0].Points[0][1], Equals, "QueryEngine")
			c.Assert(series[0].Points[0][5], Equals, float64(2.0))
			c.Assert(series[0].Points[0][6], Equals, float64(2.0))
		}
}

// issue #462 (wasn't an issue, added for regression testing only)
func (self *DataTestSuite) RegexWithDollarSign(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
[
  {
    "points": [
        ["one", 1]
    ],
    "name": "test.negative.in.where.clause.1",
    "columns": ["val_1", "val_2"]
  },
  {
    "points": [
        ["two", 2]
    ],
    "name": "test.negative.in.where.clause.1.2",
    "columns": ["val_1", "val_2"]
  },
  {
    "points": [
        ["three", 3]
    ],
    "name": "test.negative.in.where.clause.a",
    "columns": ["val_1", "val_2"]
  }
]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery(`select * from /test\.negative\.in\.where\.clause\.\d$/ limit 1`, c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "test.negative.in.where.clause.1")
			c.Assert(series[0].Columns, HasLen, 4)
			maps := ToMap(series[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["val_1"], Equals, "one")
			c.Assert(maps[0]["val_2"], Equals, 1.0)
		}
}

// https://groups.google.com/forum/#!searchin/influxdb/INT_VALUE%7Csort:relevance%7Cspell:false/influxdb/9bQAuWUnDf4/cp0vtmEe65oJ
func (self *DataTestSuite) NegativeNumbersInWhereClause(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["one", -1],
        ["two", 3]
    ],
    "name": "test_negative_in_where_clause",
    "columns": ["val_1", "val_2"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("select * from test_negative_in_where_clause where val_2 = -1 limit 1", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "test_negative_in_where_clause")
			c.Assert(series[0].Columns, HasLen, 4) // 6 columns plus the time column
			maps := ToMap(series[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["val_2"], Equals, -1.0)
			c.Assert(maps[0]["val_1"], Equals, "one")
		}
}

func (self *DataTestSuite) ExplainsWithPassthroughAndLimit(c *C) (Fun, Fun) {
	return func(client Client) {
			points := []string{}
			for i := 0; i < 101; i++ {
				points = append(points, fmt.Sprintf(`["val1", %d]`, i))
			}

			data := fmt.Sprintf(`
  [{
    "points": [%s],
    "name": "test_explain_passthrough_limit",
    "columns": ["val_1", "val_2"]
  }]`, strings.Join(points, ","))

			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("explain select val_1 from test_explain_passthrough_limit where time > now() - 1h limit 1", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "explain query")
			c.Assert(series[0].Columns, HasLen, 7) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 1)
			c.Assert(series[0].Points[0][1], Equals, "QueryEngine")

			// we can read at most point-batch-size points, which is set to 100
			// by default
			c.Assert(series[0].Points[0][5], Equals, float64(100.0))
			c.Assert(series[0].Points[0][6], Equals, float64(1.0))
		}
}

func (self *DataTestSuite) ExplainsWithNonLocalAggregator(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["val1", 2],
        ["val1", 3],
        ["val1", 4]
    ],
    "name": "test_explain_non_local",
    "columns": ["val_1", "val_2"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("explain select count(val_1) from test_explain_non_local where time > now() - 1h", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "explain query")
			c.Assert(series[0].Columns, HasLen, 7) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 1)
			c.Assert(series[0].Points[0][1], Equals, "QueryEngine")
			c.Assert(series[0].Points[0][5], Equals, float64(3.0))
			c.Assert(series[0].Points[0][6], Equals, float64(1.0))
		}
}

func (self *DataTestSuite) DistinctWithLimit(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_count_distinct_limit", 1, 1000)
			client.WriteData(data, c)
		}, func(client Client) {
			series := client.RunQuery("select distinct(column0) from test_count_distinct_limit limit 10", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Columns, HasLen, 2) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 10)
		}
}

func (self *DataTestSuite) ExplainsWithNonLocalAggregatorAndRegex(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["val1", 2],
        ["val1", 3],
        ["val1", 4]
    ],
    "name": "test_explain_non_local_regex",
    "columns": ["val_1", "val_2"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("explain select count(val_1) from /.*test_explain_non_local_regex.*/ where time > now() - 1h", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "explain query")
			c.Assert(series[0].Columns, HasLen, 7) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 1)
			c.Assert(series[0].Points[0][1], Equals, "QueryEngine")
			c.Assert(series[0].Points[0][5], Equals, float64(3.0))
			c.Assert(series[0].Points[0][6], Equals, float64(1.0))
		}
}

func (self *DataTestSuite) ExplainsWithLocalAggregator(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["val1", 2],
        ["val1", 3],
        ["val1", 4]
    ],
    "name": "test_local_aggregator",
    "columns": ["val_1", "val_2"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("explain select count(val_1) from test_local_aggregator group by time(1h) where time > now() - 1h", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "explain query")
			c.Assert(series[0].Columns, HasLen, 7) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 1)
			c.Assert(series[0].Points[0][1], Equals, "QueryEngine")
			c.Assert(series[0].Points[0][5], Equals, float64(3.0))
			c.Assert(series[0].Points[0][6], Equals, float64(1.0))
		}
}

func (self *DataTestSuite) ExplainsWithLocalAggregatorAndRegex(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
[
  {
    "points": [
        ["val1", 2],
        ["val1", 3],
        ["val1", 4]
    ],
    "name": "test_local_aggregator_regex_1",
    "columns": ["val_1", "val_2"]
  },
  {
    "points": [
        ["val1", 2],
        ["val1", 3],
        ["val1", 4]
    ],
    "name": "test_local_aggregator_regex_2",
    "columns": ["val_1", "val_2"]
  }
]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			series := client.RunQuery("explain select count(val_1) from /.*test_local_aggregator_regex.*/ group by time(1h) where time > now() - 1h", c, "m")
			c.Assert(series, HasLen, 1)
			c.Assert(series[0].Name, Equals, "explain query")
			c.Assert(series[0].Columns, HasLen, 7) // 6 columns plus the time column
			c.Assert(series[0].Points, HasLen, 2)
			c.Assert(series[0].Points[0][1], Equals, "QueryEngine")
			c.Assert(series[0].Points[0][5], Equals, 6.0)
			c.Assert(series[0].Points[0][6], Equals, 2.0)
		}
}

func (self *DataTestSuite) Medians(c *C) (Fun, Fun) {
	return func(client Client) {
			for i := 0; i < 3; i++ {
				client.WriteJsonData(fmt.Sprintf(`
[
  {
     "name": "test_medians",
     "columns": ["cpu", "host"],
     "points": [[%d, "hosta"], [%d, "hostb"]]
  }
]
`, 60+i*10, 70+i*10), c)
			}
		}, func(client Client) {
			data := client.RunQuery("select median(cpu) from test_medians group by host;", c, "m")
			c.Assert(data[0].Name, Equals, "test_medians")
			c.Assert(data[0].Columns, HasLen, 3)
			c.Assert(data[0].Points, HasLen, 2)
			medians := map[float64]string{}
			for _, point := range data[0].Points {
				medians[point[1].(float64)] = point[2].(string)
			}
			c.Assert(medians, DeepEquals, map[float64]string{70.0: "hosta", 80.0: "hostb"})
		}
}

// issue #34
func (self *DataTestSuite) AscendingQueries(c *C) (Fun, Fun) {
	return func(client Client) {
			now := time.Now().Truncate(time.Hour)
			series := &influxdb.Series{
				Name:    "test_ascending",
				Columns: []string{"host", "time"},
				Points: [][]interface{}{
					[]interface{}{"hosta", now.Add(-4 * time.Second).Unix()},
				},
			}
			client.WriteData([]*influxdb.Series{series}, c, "s")
			series = &influxdb.Series{
				Name:    "test_ascending",
				Columns: []string{"host", "time", "cpu"},
				Points: [][]interface{}{
					[]interface{}{"hosta", now.Add(-time.Second).Unix(), 60},
					[]interface{}{"hostb", now.Add(-time.Second).Unix(), 70},
				},
			}
			client.WriteData([]*influxdb.Series{series}, c, "s")

		}, func(client Client) {

			data := client.RunQuery("select host, cpu from test_ascending order asc", c, "s")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Name, Equals, "test_ascending")
			c.Assert(data[0].Columns, HasLen, 4)
			c.Assert(data[0].Points, HasLen, 3)
			maps := ToMap(data[0])
			for i := 1; i < 3; i++ {
				c.Assert(maps[i]["cpu"], NotNil)
			}
		}
}

func (self *DataTestSuite) FilterWithInvalidCondition(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_invalid_where_condition", 1, 1)
			client.WriteData(data, c)
		}, func(client Client) {
			data := client.RunQuery("select cpu from test_invalid_where_condition where column0 > 0.1s", c, "m")
			// TODO: this should return an error
			c.Assert(data, HasLen, 0)
		}
}

// issue #55
func (self *DataTestSuite) FilterWithLimit(c *C) (Fun, Fun) {
	return func(client Client) {
			now := time.Now().Truncate(time.Hour)
			series := &influxdb.Series{
				Name:    "test_ascending",
				Columns: []string{"host", "time", "cpu"},
				Points: [][]interface{}{
					[]interface{}{"hosta", now.Add(-time.Second).Unix(), 60},
					[]interface{}{"hostb", now.Add(-time.Second).Unix(), 70},
					[]interface{}{"hosta", now.Add(-2 * time.Second).Unix(), 70},
					[]interface{}{"hostb", now.Add(-2 * time.Second).Unix(), 80},
				},
			}
			client.WriteData([]*influxdb.Series{series}, c)
		}, func(client Client) {
			data := client.RunQuery("select host, cpu from test_ascending where host = 'hostb' order asc limit 1", c, "m")
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["cpu"], Equals, 80.0)
		}
}

// issue #81
func (self *DataTestSuite) FilterWithInClause(c *C) (Fun, Fun) {
	return func(client Client) {
			series := &influxdb.Series{
				Name:    "test_in_clause",
				Columns: []string{"host", "cpu"},
				Points: [][]interface{}{
					[]interface{}{"hosta", 60},
					[]interface{}{"hostb", 70},
				},
			}
			client.WriteData([]*influxdb.Series{series}, c)
		}, func(client Client) {
			data := client.RunQuery("select host, cpu from test_in_clause where host in ('hostb')", c, "m")
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["host"], Equals, "hostb")
		}
}

// issue #85
// querying a time series shouldn't add non existing columns
func (self *DataTestSuite) Issue85(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_issue_85", 1, 1)
			client.WriteData(data, c)
		}, func(client Client) {
			_ = client.RunQuery("select new_column from test_issue_85", c, "m")
			data := client.RunQuery("select * from test_issue_85", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Columns, HasLen, 3)
		}
}

// issue #92
// grouping my multiple columns fails
// Assuming the following sample data
//
// time        | fr      | to       | app        | kb
// -----------------------------------------------------
//  now() - 1hr | home    | office   | ssl        | 10
//  now() - 1hr | home    | office   | ssl        | 20
//  now() - 1hr | home    | internet | http       | 30
//  now() - 1hr | home    | office   | http       | 40
//  now()       | home    | internet | skype      | 50
//  now()       | home    | office   | lotus      | 60
//  now()       | home    | internet | skype      | 70
//
// the query `select sum(kb) from test group by time(1h), to, app`
// will cause an index out of range
func (self *DataTestSuite) Issue92(c *C) (Fun, Fun) {
	return func(client Client) {
			hourAgo := time.Now().Add(-1 * time.Hour).Unix()
			now := time.Now().Unix()

			client.WriteJsonData(fmt.Sprintf(`
[
  {
     "name": "test_issue_92",
     "columns": ["time", "fr", "to", "app", "kb"],
     "points": [
			 [%d, "home", "office", "ssl", 10],
			 [%d, "home", "office", "ssl", 20],
			 [%d, "home", "internet", "http", 30],
			 [%d, "home", "office", "http", 40],
			 [%d, "home", "internet", "skype", 50],
			 [%d, "home", "office", "lotus", 60],
			 [%d, "home", "internet", "skype", 70]
		 ]
  }
]
`, hourAgo, hourAgo, hourAgo, hourAgo, now, now, now), c, "s")
		}, func(client Client) {
			data := client.RunQuery("select sum(kb) from test_issue_92 group by time(1h), to, app", c, "m")
			c.Assert(data, HasLen, 1)
			points := ToMap(data[0])
			// use a map since the order isn't guaranteed
			sumToPointsMap := map[float64][]map[string]interface{}{}
			for _, point := range points {
				sum := point["sum"].(float64)
				sumToPointsMap[sum] = append(sumToPointsMap[sum], point)
			}
			c.Assert(sumToPointsMap[120.0], HasLen, 1)
			c.Assert(sumToPointsMap[120.0][0]["to"], Equals, "internet")
			c.Assert(sumToPointsMap[120.0][0]["app"], Equals, "skype")
			c.Assert(sumToPointsMap[60.0], HasLen, 1)
			c.Assert(sumToPointsMap[40.0], HasLen, 1)
			c.Assert(sumToPointsMap[30.0], HasLen, 2)
		}
}

// issue #89
// Group by combined with where clause doesn't work
//
// a | b | c
// ---------
// x | y | 10
// x | y | 20
// y | z | 30
// x | z | 40
//
// `select sum(c) from test group by b where a = 'x'` should return the following:
//
// time | sum | b
// --------------
// tttt | 30  | y
// tttt | 40  | z
func (self *DataTestSuite) Issue89(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
     "name": "test_issue_89",
     "columns": ["a", "b", "c"],
     "points": [
			 ["x", "y", 10],
			 ["x", "y", 20],
			 ["y", "z", 30],
			 ["x", "z", 40]
		 ]
  }
]`, c)
		}, func(client Client) {
			data := client.RunQuery("select sum(c) from test_issue_89 group by b where a = 'x'", c, "m")
			c.Assert(data, HasLen, 1)
			points := ToMap(data[0])
			c.Assert(points, HasLen, 2)
			sums := map[string]float64{}
			for _, p := range points {
				sums[p["b"].(string)] = p["sum"].(float64)
			}
			c.Assert(sums, DeepEquals, map[string]float64{"y": 30.0, "z": 40.0})
		}
}

// issue #306
func (self *DataTestSuite) NegativeTimeInterval(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
     "name": "test_negative_interval",
     "columns": ["cpu", "host", "time"],
     "points": [[60, "hosta", -1], [70, "hostb", -2]]
  }
]
`, c)
		}, func(client Client) {
			data := client.RunQuery("select count(cpu) from test_negative_interval", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Name, Equals, "test_negative_interval")
			c.Assert(data[0].Columns, HasLen, 2)
			c.Assert(data[0].Points, HasLen, 1)
			// count should be 3
			c.Assert(data[0].Points[0][1], Equals, 2.0)
		}
}

// issue #306
func (self *DataTestSuite) ShardBoundaries(c *C) (Fun, Fun) {
	return func(client Client) {
			d := `
[
  {
     "name": "test_end_time_of_shard_is_exclusive",
     "columns": ["cpu", "host", "time"],
     "points": [[60, "hosta", -1], [70, "hostb", 0]]
  }
]
`
			client.WriteJsonData(d, c, "s")
		}, func(client Client) {
			for _, query := range []string{
				"select count(cpu) from test_end_time_of_shard_is_exclusive where time > 0s",
				"select count(cpu) from test_end_time_of_shard_is_exclusive where time < 0s",
			} {
				fmt.Printf("Running query: %s\n", query)
				data := client.RunQuery(query, c, "s")
				c.Assert(data, HasLen, 1)
				c.Assert(data[0].Name, Equals, "test_end_time_of_shard_is_exclusive")
				c.Assert(data[0].Columns, HasLen, 2)
				c.Assert(data[0].Points, HasLen, 1)
				c.Assert(data[0].Points[0][1], Equals, 1.0)
			}
		}
}

// make sure aggregation when happen locally at the shard level don't
// get repeated at the coordinator level, otherwise unexpected
// behavior will happen
func (self *DataTestSuite) CountWithGroupByTimeAndLimit(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_count_with_groupby_and_limit", 1, 2)
			client.WriteData(data, c)
		}, func(client Client) {
			data := client.RunQuery("select count(column0) from test_count_with_groupby_and_limit group by time(5m) limit 10", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Name, Equals, "test_count_with_groupby_and_limit")
			c.Assert(data[0].Columns, HasLen, 2)
			c.Assert(data[0].Points, HasLen, 1)
			c.Assert(data[0].Points[0][1], Equals, 2.0)
		}
}

func (self *DataTestSuite) WhereQuery(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[4], [10], [5]], "name": "test_where_query", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select * from test_where_query where value < 6", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 2)
			c.Assert(maps[0]["value"], Equals, 5.0)
			c.Assert(maps[1]["value"], Equals, 4.0)
		}
}

func (self *DataTestSuite) CountWithGroupBy(c *C) (Fun, Fun) {
	return func(client Client) {
			series := &influxdb.Series{
				Name:    "test_count",
				Columns: []string{"host"},
			}
			for i := 0; i < 20; i++ {
				series.Points = append(series.Points, []interface{}{"hosta"})
				series.Points = append(series.Points, []interface{}{"hostb"})
			}
			client.WriteData([]*influxdb.Series{series}, c)
		}, func(client Client) {
			data := client.RunQuery("select count(host) from test_count group by host limit 10", c, "m")
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps[0]["count"], Equals, 20.0)
		}
}

func (self *DataTestSuite) CountWithAlias(c *C) (Fun, Fun) {
	return func(client Client) {
			// generate 10 points to make sure the aggregation will be calculated
			now := time.Now().Truncate(time.Hour)
			data := CreatePoints("test_aliasing", 1, 2)
			data[0].Columns = append(data[0].Columns, "time")
			data[0].Points[0] = append(data[0].Points[0], now.Unix())
			data[0].Points[1] = append(data[0].Points[1], now.Add(-time.Second).Unix())
			client.WriteData(data, c)
		}, func(client Client) {
			for _, name := range engine.GetRegisteredAggregators() {
				query := fmt.Sprintf("select %s(column0) as some_alias from test_aliasing", name)
				if name == "percentile" {
					query = "select percentile(column0, 90) as some_alias from test_aliasing"
				}
				fmt.Printf("query: %s\n", query)
				data := client.RunQuery(query, c, "m")
				c.Assert(data, HasLen, 1)
				c.Assert(data[0].Name, Equals, "test_aliasing")
				if name == "histogram" {
					c.Assert(data[0].Columns, DeepEquals, []string{"time", "some_alias_bucket_start", "some_alias_count"})
					continue
				}
				c.Assert(data[0].Columns, DeepEquals, []string{"time", "some_alias"})
			}
		}
}

// test for issue #30
func (self *DataTestSuite) HttpPostWithTime(c *C) (Fun, Fun) {
	return func(client Client) {
			now := time.Now().Add(-10 * 24 * time.Hour)
			data := CreatePoints("test_post_with_time", 1, 1)
			data[0].Columns = append(data[0].Columns, "time")
			data[0].Points[0] = append(data[0].Points[0], now.Unix())
			client.WriteData(data, c, "s")
		}, func(client Client) {
			data := client.RunQuery("select * from test_post_with_time where time > now() - 20d", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Columns, HasLen, 3)
			c.Assert(data[0].Points, HasLen, 1)
		}
}

// test limit when getting data from multiple shards
func (self *DataTestSuite) LimitMultipleShards(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
    "name": "test_limit_with_multiple_shards",
    "columns": ["time", "a"],
    "points":[
		  [1393577978000, 1],
		  [1383577978000, 2],
		  [1373577978000, 2],
		  [1363577978000, 2],
		  [1353577978000, 2],
		  [1343577978000, 2],
		  [1333577978000, 2],
		  [1323577978000, 2],
		  [1313577978000, 2]
	  ]
  }
]`, c, "m")
		}, func(client Client) {
			data := client.RunQuery("select * from test_limit_with_multiple_shards limit 1", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Points, HasLen, 1)
		}
}

// test for issue #106
func (self *DataTestSuite) Issue106(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_issue_106", 1, 1)
			client.WriteData(data, c)
		}, func(client Client) {
			data := client.RunQuery("select derivative(column0) from test_issue_106", c, "m")
			c.Assert(data, HasLen, 0)
		}
}

func (self *DataTestSuite) Issue105(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
    "name": "test_issue_105",
    "columns": ["time", "a", "b"],
    "points":[
		  [1386262529794, 2, 1],
		  [1386262529794, 2, null]
	  ]
  }
]`, c, "m")
		}, func(client Client) {

			data := client.RunQuery("select a, b from test_issue_105 where b > 0", c, "m")
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["b"], Equals, 1.0)
		}
}

func (self *DataTestSuite) WhereConditionWithExpression(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
    "name": "test_where_expression",
    "columns": ["time", "a", "b"],
    "points":[
		  [1386262529794, 2, 1],
		  [1386262529794, 2, 0]
	  ]
  }
]`, c, "m")
		}, func(client Client) {
			data := client.RunQuery("select a, b from test_where_expression where a + b >= 3", c, "m")
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["b"], Equals, 1.0)
		}
}

func (self *DataTestSuite) AggregateWithExpression(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
    "name": "test_aggregate_expression",
    "columns": ["time", "a", "b"],
    "points":[
		  [1386262529794, 1, 1],
		  [1386262529794, 2, 2]
	  ]
  }
]`, c, "m")
		}, func(client Client) {
			data := client.RunQuery("select mean(a + b) from test_aggregate_expression", c, "m")
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["mean"], Equals, 3.0)
		}
}

func (self *DataTestSuite) verifyWrite(series string, value interface{}, c *C) (Fun, Fun) {
	return func(client Client) {
			series := CreatePoints("foo", 1, 1)
			series[0].Columns = append(series[0].Columns, "time", "sequence_number")
			now := time.Now().Truncate(time.Hour)
			series[0].Points[0] = append(series[0].Points[0], 1.0, now.Unix())
			client.WriteData(series, c, "s")
			series[0].Points[0][0] = value
			client.WriteData(series, c, "s")
		}, func(client Client) {
			data := client.RunQuery("select * from foo", c, "m")
			if value == nil {
				c.Assert(data, HasLen, 0)
				return
			}
			c.Assert(data, HasLen, 1)
			maps := ToMap(data[0])
			c.Assert(maps[0]["column0"], Equals, value)
		}
}

func (self *DataTestSuite) UpdatePoint(c *C) (Fun, Fun) {
	return self.verifyWrite("test_updating_point", 1.0, c)
}

func (self *DataTestSuite) DeletePoint(c *C) (Fun, Fun) {
	return self.verifyWrite("test_deleting_point", nil, c)
}

func (self *DataTestSuite) InvalidDeleteQuery(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_invalid_delete_query", 1, 1)
			client.WriteData(data, c)
		}, func(client Client) {
			_ = client.RunInvalidQuery("delete from test_invalid_delete_query where foo = 'bar'", c, "m")
			data := client.RunQuery("select * from test_invalid_delete_query", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Points, HasLen, 1)
		}
}

func (self *DataTestSuite) ReadingWhenColumnHasDot(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
     "name": "test_column_names_with_dots",
     "columns": ["first.name", "last.name"],
     "points": [["paul", "dix"], ["john", "shahid"]]
  }
]`, c)
		}, func(client Client) {
			for name, expected := range map[string]map[string]bool{
				"first.name": map[string]bool{"paul": true, "john": true},
				"last.name":  map[string]bool{"dix": true, "shahid": true},
			} {
				q := fmt.Sprintf("select %s from test_column_names_with_dots", name)

				data := client.RunQuery(q, c, "m")
				c.Assert(data, HasLen, 1)
				c.Assert(data[0].Columns, HasLen, 3) // time, sequence number and the requested columns
				c.Assert(data[0].Columns[2], Equals, name)
				names := map[string]bool{}
				for _, p := range data[0].Points {
					names[p[2].(string)] = true
				}
				c.Assert(names, DeepEquals, expected)
			}
		}
}

func (self *DataTestSuite) SinglePointSelect(c *C) (Fun, Fun) {
	return func(client Client) {
			data := CreatePoints("test_single_points", 1, 2)
			client.WriteData(data, c)
		}, func(client Client) {

			query := "select * from test_single_points;"
			data := client.RunQuery(query, c, "u")
			c.Assert(data[0].Points, HasLen, 2)

			for _, point := range data[0].Points {
				query := fmt.Sprintf("select * from test_single_points where time = %.0f and sequence_number = %0.f;", point[0].(float64), point[1])
				data := client.RunQuery(query, c, "u")
				c.Assert(data, HasLen, 1)
				c.Assert(data[0].Points, HasLen, 1)
				c.Assert(data[0].Points[0], HasLen, 3)
				c.Assert(data[0].Points[0][2], Equals, point[2])
			}
		}
}

func (self *DataTestSuite) SinglePointSelectWithNullValues(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
     "name": "test_single_points_with_nulls",
     "columns": ["name", "age"],
     "points": [["paul", 50],["john", null]]
  }
]`, c)
		}, func(client Client) {

			query := "select * from test_single_points_with_nulls where name='john';"
			data := client.RunQuery(query, c, "u")
			c.Assert(data[0].Points, HasLen, 1)

			for _, point := range data[0].Points {
				query := fmt.Sprintf("select * from test_single_points_with_nulls where time = %.0f and sequence_number = %0.f;", point[0].(float64), point[1])
				data := client.RunQuery(query, c, "u")
				c.Assert(data, HasLen, 1)
				c.Assert(data[0].Points, HasLen, 1)
				c.Assert(data[0].Points[0], HasLen, 3)
				c.Assert(data[0].Points[0][2], Equals, point[3])
			}
		}
}

func (self *DataTestSuite) BooleanColumnsWorkWithWhereQuery(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
    "name": "test_boolean_columns_where",
    "columns": ["a"],
    "points":[[true], [false], [true]]
  }
]`, c)
		}, func(client Client) {
			data := client.RunQuery("select count(a) from test_boolean_columns_where where a = true", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Points, HasLen, 1)
			c.Assert(data[0].Points[0][1], Equals, 2.0)
		}
}

func (self *DataTestSuite) ColumnsWithOnlySomeValuesWorkWithWhereQuery(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
    "name": "test_missing_column_values",
    "columns": ["a", "b"],
    "points":[["a", "b"]]
  }
]`, c)
			client.WriteJsonData(`
[
  {
    "name": "test_missing_column_values",
    "columns": ["a", "b", "c"],
    "points":[["a", "b", "c"]]
  }
]`, c)
		}, func(client Client) {
			data := client.RunQuery("select * from test_missing_column_values where c = 'c'", c, "m")
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Points, HasLen, 1)
		}
}

func (self *DataTestSuite) SeriesListing(c *C) (Fun, Fun) {
	return func(client Client) {
			client.WriteJsonData(`
[
  {
     "name": "test_series_listing",
     "columns": ["cpu", "host"],
     "points": [[99.2, "hosta"], [55.6, "hostb"]]
  }
]
`, c)
		}, func(client Client) {
			data := client.RunQuery("list series", c, "m")
			names := map[string]bool{}
			for _, series := range data {
				names[series.Name] = true
			}
			c.Assert(names["test_series_listing"], Equals, true)
		}
}

func (self *DataTestSuite) ArithmeticOperations(c *C) (Fun, Fun) {
	queries := map[string][9]float64{
		"select input + output from test_arithmetic_3.0;":       [9]float64{1, 2, 3, 4, 5, 9, 6, 7, 13},
		"select input - output from test_arithmetic_-1.0;":      [9]float64{1, 2, -1, 4, 5, -1, 6, 7, -1},
		"select input * output from test_arithmetic_2.0;":       [9]float64{1, 2, 2, 4, 5, 20, 6, 7, 42},
		"select 1.0 * input / output from test_arithmetic_0.5;": [9]float64{1, 2, 0.5, 4, 5, 0.8, 6, 8, 0.75},
	}

	return func(client Client) {
			for query, values := range queries {

				fmt.Printf("Running query %s\n", query)

				for i := 0; i < 3; i++ {

					data := fmt.Sprintf(`
        [
          {
             "name": "test_arithmetic_%.1f",
             "columns": ["input", "output"],
             "points": [[%f, %f]]
          }
        ]
      `, values[2], values[3*i], values[3*i+1])
					client.WriteJsonData(data, c)
				}
			}

		}, func(client Client) {

			for query, values := range queries {

				data := client.RunQuery(query, c, "m")
				c.Assert(data, HasLen, 1)
				c.Assert(data[0].Columns, HasLen, 3)
				c.Assert(data[0].Points, HasLen, 3)
				for i, p := range data[0].Points {
					idx := 2 - i
					c.Assert(p[2], Equals, values[3*idx+2])
				}
			}
		}
}

// issue #437
func (self *DataTestSuite) ConstantsInArithmeticQueries(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[1]], "name": "test_constants", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			for _, query := range []string{
				"select -1 * value from test_constants",
				"select -1.0 * value from test_constants",
			} {
				collection := client.RunQuery(query, c)
				c.Assert(collection, HasLen, 1)
				maps := ToMap(collection[0])
				c.Assert(maps, HasLen, 1)
				c.Assert(maps[0]["expr0"], Equals, -1.0)
			}
		}
}

func (self *DataTestSuite) CountQueryOnSingleShard(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[4], [10], [5]], "name": "test_count_query_single_shard", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
			t := time.Now().Add(-time.Minute)
			data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_count_query_single_shard", "columns": ["value", "time"]}]`, t.Unix())
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			collection := client.RunQuery("select count(value) from test_count_query_single_shard group by time(1m)", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 2)
			c.Assert(maps[0]["count"], Equals, 3.0)
			c.Assert(maps[1]["count"], Equals, 1.0)
		}
}

func (self *DataTestSuite) GroupByDay(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[4], [10], [5]], "name": "test_group_by_day", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
			t := time.Now().Add(-24 * time.Hour).Unix()
			data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_group_by_day", "columns": ["value", "time"]}]`, t)
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			collection := client.RunQuery("select count(value) from test_group_by_day group by time(1d)", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 2)
			c.Assert(maps[0]["count"], Equals, 3.0)
			c.Assert(maps[1]["count"], Equals, 1.0)
		}
}

func (self *DataTestSuite) LimitQueryOnSingleShard(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[4], [10], [5]], "name": "test_limit_query_single_shard", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select * from test_limit_query_single_shard limit 2", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 2)
			c.Assert(maps[0]["value"], Equals, 5.0)
			c.Assert(maps[1]["value"], Equals, 10.0)
		}
}

func (self *DataTestSuite) QueryAgainstMultipleShards(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[4], [10], [5]], "name": "test_query_against_multiple_shards", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
			t := time.Now().Add(-14 * 24 * time.Hour).Unix()
			data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_query_against_multiple_shards", "columns": ["value", "time"]}]`, t)
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			collection := client.RunQuery("select count(value) from test_query_against_multiple_shards group by time(1h)", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 2)
			c.Assert(maps[0]["count"], Equals, 3.0)
			c.Assert(maps[1]["count"], Equals, 1.0)
		}
}

func (self *DataTestSuite) QueryAscendingAgainstMultipleShards(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"points": [[4], [10]], "name": "test_ascending_against_multiple_shards", "columns": ["value"]}]`
			client.WriteJsonData(data, c)
			t := time.Now().Add(-14 * 24 * time.Hour).Unix()
			data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_ascending_against_multiple_shards", "columns": ["value", "time"]}]`, t)
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			collection := client.RunQuery("select * from test_ascending_against_multiple_shards order asc", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 3)
			c.Assert(maps[0]["value"], Equals, 2.0)
			c.Assert(maps[1]["value"], Equals, 4.0)
			c.Assert(maps[2]["value"], Equals, 10.0)
		}
}

func (self *DataTestSuite) BigGroupByQueryAgainstMultipleShards(c *C) (Fun, Fun) {
	return func(client Client) {
			duration := int64(30 * 24 * time.Hour)
			first := time.Unix(time.Now().Unix()/duration*duration, 0).Add(7 * 24 * time.Hour)

			data := fmt.Sprintf(
				`[{"points": [[4, %d], [10, %d]], "name": "test_multiple_shards_big_group_by", "columns": ["value", "time"]}]`,
				first.Unix(), first.Unix())
			client.WriteJsonData(data, c)
			t := first.Add(7 * 24 * time.Hour).Unix()
			data = fmt.Sprintf(`[{"points": [[2, %d]], "name": "test_multiple_shards_big_group_by", "columns": ["value", "time"]}]`, t)
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			collection := client.RunQuery("select count(value) from test_multiple_shards_big_group_by group by time(30d)", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["count"], Equals, 3.0)
		}
}

func (self *DataTestSuite) WriteSplitToMultipleShards(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[
		{"points": [[4], [10]], "name": "test_write_multiple_shards", "columns": ["value"]},
		{"points": [["asdf"]], "name": "Test_write_multiple_shards", "columns": ["thing"]}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select count(value) from test_write_multiple_shards", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["count"], Equals, 2.0)

			collection = client.RunQuery("select * from Test_write_multiple_shards", c)
			c.Assert(collection, HasLen, 1)
			maps = ToMap(collection[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["thing"], Equals, "asdf")
		}
}

func (self *DataTestSuite) CountDistinctWithNullValues(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{"name":"test_null_with_distinct","columns":["column"],"points":[["value1"], [null], ["value2"], ["value1"], [null]]}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select count(distinct(column)) from test_null_with_distinct group by time(1m)", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["count"], Equals, 2.0)
		}
}

// issue #147
func (self *DataTestSuite) ExtraSequenceNumberColumns(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["foo", 1390852524, 1234]
    ],
    "name": "test_extra_sequence_number_column",
    "columns": ["val_1", "time", "sequence_number"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select * from test_extra_sequence_number_column", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps, HasLen, 1)
			c.Assert(maps[0]["sequence_number"], Equals, 1234.0)
		}
}

// issue #206
func (self *DataTestSuite) UnicodeSupport(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `
  [{
    "points": [
        ["山田太郎", "中文", "⚑"]
    ],
    "name": "test_unicode",
    "columns": ["val_1", "val_2", "val_3"]
  }]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select * from test_unicode", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			c.Assert(maps[0]["val_1"], Equals, "山田太郎")
			c.Assert(maps[0]["val_2"], Equals, "中文")
			c.Assert(maps[0]["val_3"], Equals, "⚑")
		}
}
func (self *DataTestSuite) SelectingTimeColumn(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{
		"name": "selecting_time_column",
		"columns": ["val1"],
		"points": [[1]]
		}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select val1, time from selecting_time_column", c)
			c.Assert(collection, HasLen, 1)
			c.Assert(collection[0].Columns, HasLen, 3)
			c.Assert(collection[0].Points, HasLen, 1)
		}
}

// For issue #130 https://github.com/influxdb/influxdb/issues/130
func (self *DataTestSuite) ColumnNamesReturnInDistributedQuery(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{
		"name": "cluster_query_with_columns",
		"columns": ["col1"],
		"points": [[1], [2]]
		}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select * from cluster_query_with_columns", c)
			c.Assert(collection, HasLen, 1)
			maps := ToMap(collection[0])
			set := map[float64]bool{}
			for idx, _ := range maps {
				set[maps[idx]["col1"].(float64)] = true
			}
			c.Assert(set, DeepEquals, map[float64]bool{1: true, 2: true})
		}
}
func (self *DataTestSuite) LimitWithRegex(c *C) (Fun, Fun) {
	batchSize := []int{100, 1000}
	return func(client Client) {
			// run the test once with less than POINT_BATCH_SIZE points and once
			// with more than POINT_BATCH_SIZE points
			series := []*influxdb.Series{}
			for _, numberOfPoints := range batchSize {
				for i := 0; i < 100; i++ {
					series = append(series, CreatePointsFromFunc(fmt.Sprintf("limit_with_regex_%d_%d", numberOfPoints, i), 1, numberOfPoints,
						func(i int) float64 { return float64(i) },
					)...)
				}
			}
			client.WriteData(series, c)
		}, func(client Client) {
			for _, numberOfPoints := range batchSize {
				query := fmt.Sprintf("select * from /.*limit_with_regex_%d.*/ limit 1", numberOfPoints)
				collection := client.RunQuery(query, c)
				// make sure all series get back 1 point only
				series := map[string][]map[string]interface{}{}
				for _, s := range collection {
					series[s.Name] = ToMap(s)
				}
				for i := 0; i < 100; i++ {
					table := fmt.Sprintf("limit_with_regex_%d_%d", numberOfPoints, i)
					c.Assert(series[table], HasLen, 1)
					c.Assert(series[table][0]["column0"], Equals, float64(numberOfPoints-1))
				}
			}
		}
}

// For issue #131 https://github.com/influxdb/influxdb/issues/131
func (self *DataTestSuite) SelectFromRegexInCluster(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[{
		"name": "cluster_regex_query",
		"columns": ["col1", "col2"],
		"points": [[1, "foo"], [23, "bar"]]
		},{
			"name": "cluster_regex_query_2",
			"columns": ["blah"],
			"points": [[true]]
		},{
			"name": "cluster_regex_query_3",
			"columns": ["foobar"],
			"points": [["asdf"]]
			}]`
			client.WriteJsonData(data, c)
		}, func(client Client) {
			collection := client.RunQuery("select * from /.*/ limit 1", c)
			c.Assert(collection, HasLen, 3)
			series := map[string][]map[string]interface{}{}
			for _, s := range collection {
				series[s.Name] = ToMap(s)
			}
			c.Assert(series["cluster_regex_query"], HasLen, 1)
			c.Assert(series["cluster_regex_query"][0]["col1"], Equals, 23.0)
			c.Assert(series["cluster_regex_query"][0]["col2"], Equals, "bar")
			c.Assert(series["cluster_regex_query_2"], HasLen, 1)
			c.Assert(series["cluster_regex_query_2"][0]["blah"], Equals, true)
			c.Assert(series["cluster_regex_query_3"], HasLen, 1)
			c.Assert(series["cluster_regex_query_3"][0]["foobar"], Equals, "asdf")
		}
}

func (self *DataTestSuite) ListSeries(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[
        {"points": [[1]], "name": "cluster_query", "columns": ["value"]},
			  {"points": [[2]], "name": "another_query", "columns": ["value"]}
      ]`
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			collection := client.RunQuery("list series", c)
			c.Assert(collection, HasLen, 2)
			names := map[string]bool{}
			for _, s := range collection {
				c.Assert(s.Points, HasLen, 0)
				names[s.Name] = true
			}
			c.Assert(names["cluster_query"], Equals, true)
			c.Assert(names["another_query"], Equals, true)
		}
}

// For issue #267 - allow all characters in series name - https://github.com/influxdb/influxdb/issues/267
func (self *SingleServerSuite) SeriesNameWithWeirdCharacters(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[
  	  	{
	      	"name": "/blah ( ) ; : ! @ # $ \n \t,foo\"=bar/baz",
      		"columns": ["value"],
    	  	"points": [[1]]
  	  	}
	  	]`
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			data := client.RunQuery("select value from \"/blah ( ) ; : ! @ # $ \n \t,foo\\\"=bar/baz\"", c)
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Points, HasLen, 1)
			c.Assert(data[0].Name, Equals, "/blah ( ) ; : ! @ # $ \n \t,foo\"=bar/baz")
		}
}

// For issue #466 - allow all characters in column names - https://github.com/influxdb/influxdb/issues/267
func (self *SingleServerSuite) ColumnNameWithWeirdCharacters(c *C) (Fun, Fun) {
	return func(client Client) {
			data := `[
  	  	{
	      	"name": "foo",
      		"columns": ["foo.-239(*@&#$!#)(* #$@"],
    	  	"points": [[1]]
  	  	}
	  	]`
			client.WriteJsonData(data, c, "s")
		}, func(client Client) {
			data := client.RunQuery("select \"foo.-239(*@&#$!#)(* #$@\" from foo", c)
			c.Assert(data, HasLen, 1)
			c.Assert(data[0].Points, HasLen, 1)
			c.Assert(data[0].Name, Equals, "foo")
			c.Assert(data[0].Columns, HasLen, 1)
			c.Assert(data[0].Columns[0], Equals, "foo.-239(*@&#$!#)(* #$@")
		}
}
