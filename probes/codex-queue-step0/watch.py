import sqlite3,sys,time
q=sys.argv[1]; dur=float(sys.argv[2]); t0=time.time(); last=None
while time.time()-t0<dur:
    try:
        c=sqlite3.connect('file:%s?mode=ro'%q,uri=True,timeout=2); r=[(x[0][-4:],x[1][-4:],x[2]) for x in c.execute('select id,thread_id,queue_order from queued_items order by thread_id,queue_order')]; c.close()
    except Exception as e: r=str(e)
    if r!=last: print("%.2f rows=%s"%(time.time()-t0,r),flush=True); last=r
    time.sleep(0.2)
