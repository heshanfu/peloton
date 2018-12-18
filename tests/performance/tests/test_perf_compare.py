import unittest
from StringIO import StringIO
from tests.performance.perf_compare import (
    parse_arguments,
    compare_create,
    compare_get,
    compare_update
)
import pandas as pd

CREATE_DF_1 = """
\tCores\tTaskNum\tSleep(s)\tUseInsConf\tVersion\tStart(s)\tExec(s)
0\t1000\t5000\t10\tTrue\t0.6.9-86-g7135a0e\t3.97567\t24.73733
"""

CREATE_DF_2 = """
\tCores\tTaskNum\tSleep(s)\tUseInsConf\tVersion\tStart(s)\tExec(s)
0\t1000\t5000\t10\tTrue\t0.6.11-5-gb4a1c68\t4.31115\t20.98786
"""

GET_DF_1 = """
\tTaskNum\tSleep(s)\tUseInsConf\tCreates\tVersion\tCreateFails\tGets\tGetFails
0\t5000\t10\t70\t0.7.8\t3\t73\t0
"""

GET_DF_2 = """
\tTaskNum\tSleep(s)\tUseInsConf\tCreates\tVersion\tCreateFails\tGets\tGetFails
0\t5000\t10\t70\t0.7.8-CURRENT\t3\t60\t13
"""

UPDATE_DF_1 = """
\tNumStartTasks\tTaskIncrementEachTime\tNumOfIncrement\tSleep(s)\tUseInsConf\tVersion\tTotalTimeInSeconds
5\t1\t1\t5000\t10\t0.7.9\t850
"""

UPDATE_DF_2 = """
\tNumStartTasks\tTaskIncrementEachTime\tNumOfIncrement\tSleep(s)\tUseInsConf\tVersion\tTotalTimeInSeconds
5\t1\t1\t5000\t10\t0.7.9-CURRENT\t950
"""


class PerfCompareTest(unittest.TestCase):
    def test_parser(self):
        parser = parse_arguments(['-f1', 'PERF_1', '-f2', 'PERF_2'])
        self.assertEqual(parser.file_1, 'PERF_1')
        self.assertEqual(parser.file_2, 'PERF_2')

    def test_compare_create(self):
        df1 = pd.read_csv(StringIO(CREATE_DF_1), '\t', index_col=0)
        df2 = pd.read_csv(StringIO(CREATE_DF_2), '\t', index_col=0)

        df_out = compare_create(df1, df2)
        self.assertEqual(df_out.iloc[0]['Perf Change'], '-0.1516')

    def test_compare_get(self):
        df1 = pd.read_csv(StringIO(GET_DF_1), '\t', index_col=0)
        df2 = pd.read_csv(StringIO(GET_DF_2), '\t', index_col=0)
        df_out = compare_get(df1, df2)

        shared_fields = ['TaskNum', 'Sleep(s)', 'UseInsConf', 'Creates']
        for field in shared_fields:
            self.assertEqual(df_out.iloc[0][field],
                             df_out.iloc[1][field])
        self.assertEqual(df_out.iloc[0]['Version_x'], '0.7.8')
        self.assertEqual(df_out.iloc[0]['Version_y'], '0.7.8-CURRENT')

    def test_compare_update(self):
        df1 = pd.read_csv(StringIO(UPDATE_DF_1), '\t', index_col=0)
        df2 = pd.read_csv(StringIO(UPDATE_DF_2), '\t', index_col=0)
        df_out = compare_update(df1, df2)

        self.assertEqual(df_out.iloc[0]['Version_x'], '0.7.9')
        self.assertEqual(df_out.iloc[0]['Version_y'], '0.7.9-CURRENT')
        self.assertEqual(df_out.iloc[0]['Time Diff'], '100')
