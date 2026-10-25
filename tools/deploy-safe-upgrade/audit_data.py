#!/usr/bin/env python3
"""Compare read-only deployment snapshots without exposing stored credentials."""
import argparse,hashlib,json,pathlib,sqlite3,sys
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'dmit-wan-calibrator'))
from dmit_wan_calibrator import get_latest_counter

def digest(value):
 return hashlib.sha256(json.dumps(value,sort_keys=True,ensure_ascii=False,default=str).encode()).hexdigest()

def audit(root):
 root=pathlib.Path(root)
 db=sqlite3.connect(f'file:{root / "komari.db"}?mode=ro',uri=True)
 tables={r[0] for r in db.execute("select name from sqlite_master where type='table'")}
 result={'counts':{},'identity':{},'providers':{},'counters':{}}
 for table in ['clients','users','sessions','traffic_report_notifications','traffic_reset_reminders','ssh_login_notifications','ssh_login_events','ping_tasks','gpu_records_long_term','load_notifications']:
  if table in tables:result['counts'][table]=db.execute('select count(*) from '+table).fetchone()[0]
 cols='uuid,token,name,price,billing_cycle,currency,traffic_limit,traffic_limit_type,traffic_reset_day,traffic_reset_timezone,traffic_reset_reported,vnstat_baseline_up,vnstat_baseline_down,vnstat_baseline_vn_up,vnstat_baseline_vn_down'
 rows=db.execute('select '+cols+' from clients order by uuid').fetchall()
 result['identity']['clients']=digest(rows)
 if 'users' in tables:result['identity']['users']=digest(db.execute('select uuid,username,passwd,two_factor from users order by uuid').fetchall())
 if 'message_sender_providers' in tables:
  for name,addition in db.execute('select name,addition from message_sender_providers order by name'):result['providers'][name]=digest(addition)
 uuids=[r[0] for r in rows]
 db.close()
 metrics=sqlite3.connect(f'file:{root / "metrics.db"}?mode=ro',uri=True)
 for uuid in uuids:
  for metric in ['net.total.up','net.total.down']:
   try:
    value,ns=get_latest_counter(metrics,uuid,metric)
    result['counters'][uuid+'/'+metric]=[value,ns//1000000]
   except RuntimeError:pass
 metrics.close()
 return result

def main():
 p=argparse.ArgumentParser();p.add_argument('--source',required=True);p.add_argument('--candidate',required=True);args=p.parse_args()
 before=audit(args.source);after=audit(args.candidate)
 checks={'node_and_custom_table_counts':before['counts']==after['counts'],'identity_billing_and_anchors':before['identity']==after['identity'],'existing_notification_credentials':all(after['providers'].get(k)==v for k,v in before['providers'].items()),'latest_traffic_counters_and_timestamps':before['counters']==after['counters']}
 print(json.dumps({'checks':checks,'nodes':before['counts'].get('clients'),'traffic_counters':len(before['counters']),'passed':all(checks.values())}))
 return 0 if all(checks.values()) else 1
if __name__=='__main__':raise SystemExit(main())
