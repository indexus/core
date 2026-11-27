----------------------- MODULE SubregionAuthority -----------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS Keys, GrainSize, MaxSeq, UnsafeReselect
VARIABLES chosen, phase, fence, installed, sourceSeq, receiverSeq, ackSeq, ready, faultLeft
vars == <<chosen, phase, fence, installed, sourceSeq, receiverSeq, ackSeq, ready, faultLeft>>
(* A key denotes a geometric piece, including addresses without existing rows.
   Capture chooses one piece of an exact partition; the residual stays at S.
   Phase 0 idle, 1 copy, 2 fence, 3 final seal, 4 emit, 5 install. *)
Init == /\ chosen = {} /\ phase = 0 /\ fence = {} /\ installed = {}
 /\ sourceSeq = [k \in Keys |-> 0] /\ receiverSeq = [k \in Keys |-> 0]
 /\ ackSeq = [k \in Keys |-> 0] /\ ready = TRUE /\ faultLeft = TRUE
SourceWrite(k) == /\ k \notin fence /\ sourceSeq[k] < MaxSeq
 /\ sourceSeq' = [sourceSeq EXCEPT ![k] = @+1]
 /\ ackSeq' = [ackSeq EXCEPT ![k] = sourceSeq'[k]]
 /\ UNCHANGED <<chosen, phase, fence, installed, receiverSeq, ready, faultLeft>>
ReceiverWrite(k) == /\ k \in installed /\ receiverSeq[k] < MaxSeq
 /\ receiverSeq' = [receiverSeq EXCEPT ![k] = @+1]
 /\ ackSeq' = [ackSeq EXCEPT ![k] = receiverSeq'[k]]
 /\ UNCHANGED <<chosen, phase, fence, installed, sourceSeq, ready, faultLeft>>
Capture == /\ phase = 0 /\ installed # Keys
 /\ \E g \in SUBSET (Keys \ installed):
       /\ g # {} /\ Cardinality(g) <= GrainSize /\ chosen' = g
 /\ phase' = 1
 /\ UNCHANGED <<fence, installed, sourceSeq, receiverSeq, ackSeq, ready, faultLeft>>
Freeze == /\ phase = 1 /\ ready /\ fence' = fence \cup chosen /\ phase' = 2
 /\ UNCHANGED <<chosen, installed, sourceSeq, receiverSeq, ackSeq, ready, faultLeft>>
Seal == /\ phase = 2 /\ ready /\ phase' = 3
 /\ receiverSeq' = [k \in Keys |-> IF k \in chosen THEN sourceSeq[k] ELSE receiverSeq[k]]
 /\ UNCHANGED <<chosen, fence, installed, sourceSeq, ackSeq, ready, faultLeft>>
Emit == /\ phase = 3 /\ ready /\ phase' = 4
 /\ UNCHANGED <<chosen, fence, installed, sourceSeq, receiverSeq, ackSeq, ready, faultLeft>>
Install == /\ phase = 4 /\ ready /\ phase' = 5 /\ installed' = installed \cup chosen
 /\ UNCHANGED <<chosen, fence, sourceSeq, receiverSeq, ackSeq, ready, faultLeft>>
Finish == /\ phase = 5 /\ ready /\ phase' = 0 /\ chosen' = {}
 /\ UNCHANGED <<fence, installed, sourceSeq, receiverSeq, ackSeq, ready, faultLeft>>
Crash == /\ faultLeft /\ phase \in 2..4 /\ ready
 /\ ready' = FALSE /\ faultLeft' = FALSE
 /\ UNCHANGED <<chosen, phase, fence, installed, sourceSeq, receiverSeq, ackSeq>>
Recover == /\ ~ready /\ ready' = TRUE
 /\ (IF UnsafeReselect
       THEN \E g \in SUBSET (Keys \ installed):
                /\ g # {} /\ Cardinality(g) <= GrainSize /\ chosen' = g
       ELSE chosen' = chosen)
 /\ UNCHANGED <<phase, fence, installed, sourceSeq, receiverSeq, ackSeq, faultLeft>>
Next == (\E k \in Keys: SourceWrite(k) \/ ReceiverWrite(k))
 \/ Capture \/ Freeze \/ Seal \/ Emit \/ Install \/ Finish \/ Crash \/ Recover
Spec == Init /\ [][Next]_vars /\ WF_vars(Capture) /\ WF_vars(Freeze)
 /\ WF_vars(Seal) /\ WF_vars(Emit) /\ WF_vars(Install) /\ WF_vars(Finish) /\ WF_vars(Recover)
SingleWriter == installed \subseteq fence
ACKReadable == \A k \in Keys: IF k \in installed THEN receiverSeq[k] >= ackSeq[k]
                              ELSE sourceSeq[k] >= ackSeq[k]
ResidualWritable == \A k \in Keys \ (installed \cup chosen): k \notin fence
EventuallyCovered == <>(installed = Keys)
=============================================================================
