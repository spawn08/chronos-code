import sqlite3,json,sys,collections,bisect
db=sqlite3.connect(sys.argv[1])
users=collections.defaultdict(list); summ=collections.defaultdict(list)
for sid,t,p,ca in db.execute("select session_id,type,payload,created_at from events"):
    try: j=json.loads(p)
    except: continue
    if t=='chat_summary': summ[sid].append(ca)
    elif j.get('role')=='user' and not str(j.get('content','')).startswith('{"compressed'): users[sid].append(ca)
for d in (users,summ):
    for k in d: d[k].sort()
rows=db.execute("select session_id,output,started_at from traces where name like 'model:%' and output like '%usage%' order by session_id,started_at").fetchall()
prevst={}; c=collections.Counter(); tk=collections.Counter()
for sid,o,st in rows:
    u=json.loads(o).get('usage',{}); cw=u.get('cache_creation_tokens',0); tot=cw+u.get('cache_read_tokens',0)+u.get('input_tokens',0)
    p=prevst.get(sid); prevst[sid]=st
    if not tot or cw<5000 or cw/tot<=0.8 or p is None: continue
    def between(lst): 
        i=bisect.bisect_right(lst,p); return i<len(lst) and lst[i]<=st
    k='after compaction' if between(summ.get(sid,[])) else 'at user-turn start' if between(users.get(sid,[])) else 'mid-turn (between tool rounds)'
    c[k]+=1; tk[k]+=cw
for k in tk: print(f"{k:32s} n={c[k]:4d} cw={tk[k]:>11,d}")
print("roles sample", collections.Counter(json.loads(p).get('role') for (p,) in db.execute("select payload from events where type='chat_message' limit 2000")))
