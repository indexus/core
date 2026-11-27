-------------------------- MODULE TokenIsolation --------------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS Tokens, Healthy, MaxSeq, SerialDisk
VARIABLES phase, jobs, fence, sourceSeq, receiverSeq, ackSeq
vars == <<phase, jobs, fence, sourceSeq, receiverSeq, ackSeq>>
(* Tokens are already certified, disjoint terminal grants. The other receiver
   stays partitioned forever; Healthy must still finish. One operation per
   token abstracts the token gate and the TX one-successor check. *)
Init == /\ phase=[t \in Tokens |-> 0] /\ jobs={} /\ fence={}
 /\ sourceSeq=[t \in Tokens |-> 0] /\ receiverSeq=[t \in Tokens |-> 0]
 /\ ackSeq=[t \in Tokens |-> 0]
Start(t) == /\ phase[t]=0 /\ (IF SerialDisk THEN jobs={} ELSE Cardinality(jobs)<2)
 /\ phase'=[phase EXCEPT ![t]=1] /\ jobs'=jobs \cup {t}
 /\ UNCHANGED <<fence,sourceSeq,receiverSeq,ackSeq>>
SourceWrite(t) == /\ t \notin fence /\ sourceSeq[t]<MaxSeq
 /\ sourceSeq'=[sourceSeq EXCEPT ![t]=@+1]
 /\ ackSeq'=[ackSeq EXCEPT ![t]=sourceSeq'[t]]
 /\ UNCHANGED <<phase,jobs,fence,receiverSeq>>
Freeze(t) == /\ phase[t]=1 /\ phase'=[phase EXCEPT ![t]=2] /\ fence'=fence \cup {t}
 /\ UNCHANGED <<jobs,sourceSeq,receiverSeq,ackSeq>>
Stage(t) == /\ phase[t]=2 /\ t=Healthy /\ phase'=[phase EXCEPT ![t]=3]
 /\ receiverSeq'=[receiverSeq EXCEPT ![t]=sourceSeq[t]]
 /\ UNCHANGED <<jobs,fence,sourceSeq,ackSeq>>
Emit(t) == /\ phase[t]=3 /\ phase'=[phase EXCEPT ![t]=4]
 /\ UNCHANGED <<jobs,fence,sourceSeq,receiverSeq,ackSeq>>
Install(t) == /\ phase[t]=4 /\ phase'=[phase EXCEPT ![t]=5]
 /\ UNCHANGED <<jobs,fence,sourceSeq,receiverSeq,ackSeq>>
ReceiverWrite(t) == /\ phase[t]>=5 /\ receiverSeq[t]<MaxSeq
 /\ receiverSeq'=[receiverSeq EXCEPT ![t]=@+1]
 /\ ackSeq'=[ackSeq EXCEPT ![t]=receiverSeq'[t]]
 /\ UNCHANGED <<phase,jobs,fence,sourceSeq>>
Finish(t) == /\ phase[t]=5 /\ phase'=[phase EXCEPT ![t]=6] /\ jobs'=jobs \ {t}
 /\ UNCHANGED <<fence,sourceSeq,receiverSeq,ackSeq>>
Next == \E t \in Tokens: Start(t) \/ SourceWrite(t) \/ Freeze(t) \/ Stage(t)
                         \/ Emit(t) \/ Install(t) \/ ReceiverWrite(t) \/ Finish(t)
Spec == Init /\ [][Next]_vars
 /\ \A t \in Tokens: WF_vars(Start(t)) /\ WF_vars(Freeze(t)) /\ WF_vars(Stage(t))
                    /\ WF_vars(Emit(t)) /\ WF_vars(Install(t)) /\ WF_vars(Finish(t))
SingleWriter == \A t \in Tokens: phase[t]>=5 => t \in fence
ACKReadable == \A t \in Tokens: IF phase[t]>=5 THEN receiverSeq[t]>=ackSeq[t]
                               ELSE sourceSeq[t]>=ackSeq[t]
IndependentProgress == <>(phase[Healthy]=6)
=============================================================================
