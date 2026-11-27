---------------------- MODULE FirstFenceCancellation ----------------------
EXTENDS Naturals, TLC
CONSTANT UnsafeCancelAfterPointFence
VARIABLES phase, cancelled, fence, receiverPoint, receiverNative, slot, online
vars == <<phase, cancelled, fence, receiverPoint, receiverNative, slot, online>>
(* One source, one receiver, one representative point inside a region.
   0=warm; 1=confirmed preload; 2=point fence; 3=point emitted;
   4=point installed; 5=region emitted; 6=region installed.
   No global routing state or timing is represented. *)
Init == /\ phase = 0 /\ cancelled = FALSE /\ fence = FALSE
        /\ receiverPoint = FALSE /\ receiverNative = FALSE
        /\ slot = FALSE /\ online = TRUE
Confirm == /\ phase = 0 /\ ~cancelled /\ online
           /\ phase' = 1 /\ slot' = TRUE
           /\ UNCHANGED <<cancelled,fence,receiverPoint,receiverNative,online>>
FreezePoint == /\ phase = 1 /\ ~cancelled /\ slot /\ online
               /\ phase' = 2 /\ fence' = TRUE
               /\ UNCHANGED <<cancelled,receiverPoint,receiverNative,slot,online>>
EmitPoint == /\ phase = 2 /\ ~cancelled /\ online
             /\ phase' = 3
             /\ UNCHANGED <<cancelled,fence,receiverPoint,receiverNative,slot,online>>
(* An emitted packet can arrive even after a broken source cancellation. *)
InstallPoint == /\ phase = 3 /\ slot /\ online
                /\ phase' = 4 /\ receiverPoint' = TRUE
                /\ UNCHANGED <<cancelled,fence,receiverNative,slot,online>>
EmitRegion == /\ phase = 4 /\ ~cancelled /\ online
              /\ phase' = 5
              /\ UNCHANGED <<cancelled,fence,receiverPoint,receiverNative,slot,online>>
InstallRegion == /\ phase = 5 /\ slot /\ online
                 /\ phase' = 6 /\ receiverNative' = TRUE /\ slot' = FALSE
                 /\ UNCHANGED <<cancelled,fence,receiverPoint,online>>
CancelSource == /\ ~cancelled /\ online
                /\ IF UnsafeCancelAfterPointFence THEN phase < 5 ELSE phase < 2
                /\ cancelled' = TRUE /\ fence' = FALSE
                /\ UNCHANGED <<phase,receiverPoint,receiverNative,slot,online>>
(* The receiver accepts the cancellation only if no point could be emitted.
   A timeout or peer disappearance cannot cancel its confirmed moving slot. *)
CancelReceiver == /\ cancelled /\ phase < 2 /\ slot /\ online
                  /\ slot' = FALSE
                  /\ UNCHANGED <<phase,cancelled,fence,receiverPoint,receiverNative,online>>
Crash == /\ online /\ online' = FALSE
         /\ UNCHANGED <<phase,cancelled,fence,receiverPoint,receiverNative,slot>>
Recover == /\ ~online /\ online' = TRUE
           /\ UNCHANGED <<phase,cancelled,fence,receiverPoint,receiverNative,slot>>
Next == Confirm \/ FreezePoint \/ EmitPoint \/ InstallPoint \/ EmitRegion
        \/ InstallRegion \/ CancelSource \/ CancelReceiver \/ Crash \/ Recover
Spec == Init /\ [][Next]_vars
SingleWriter == ~(~fence /\ (receiverPoint \/ receiverNative))
CancellationBeforeFirstFence == cancelled => phase < 2
MovingSlotRetained == (phase >= 2 /\ phase < 6) => slot
=============================================================================
