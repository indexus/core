----------------------- MODULE ReceiptAfterDeparture -----------------------
EXTENDS Naturals, FiniteSets, TLC
CONSTANTS UnsafePublishBeforeDrain, UnsafeAddressAsDrain
VARIABLES sourceNative, sourceData, pending, receiverNative, receiverData,
          nextNative, nextData, slot, everInstalled, moving, head, seen,
          done, receiverOnline, acknowledged
vars == <<sourceNative,sourceData,pending,receiverNative,receiverData,
          nextNative,nextData,slot,everInstalled,moving,head,seen,
          done,receiverOnline,acknowledged>>
(* One region, S→R, then R→P as physical MOVE or logical retirement.
   Admission is already confirmed. The last install response may be lost.
   No new old-epoch admission is allowed after departure; the production epoch
   gate must realize this assumption. everInstalled is a ghost oracle for the
   invariant, not a history ledger required by the implementation.
   Head/seen are compact membership records, not write permissions.
   initial/address/returned keep R's storage identity; moved changes it.
   Returned retains R's original durable closed-ticket namespace. *)
Init == /\ sourceNative = TRUE /\ sourceData = 1 /\ pending = FALSE
        /\ receiverNative = FALSE /\ receiverData = 0
        /\ nextNative = FALSE /\ nextData = 0 /\ slot = TRUE
        /\ everInstalled = FALSE /\ moving = FALSE
        /\ head = "initial" /\ seen = "initial" /\ done = FALSE
        /\ receiverOnline = TRUE /\ acknowledged = 1
Emit == /\ sourceNative /\ slot
        /\ sourceNative' = FALSE /\ pending' = TRUE
        /\ UNCHANGED <<sourceData,receiverNative,receiverData,nextNative,nextData,
                       slot,everInstalled,moving,head,seen,done,receiverOnline,acknowledged>>
Install == /\ pending /\ slot /\ receiverOnline
           /\ receiverNative' = TRUE /\ receiverData' = sourceData
           /\ slot' = FALSE /\ everInstalled' = TRUE
           /\ UNCHANGED <<sourceNative,sourceData,pending,nextNative,nextData,
                          moving,head,seen,done,receiverOnline,acknowledged>>
ReceiverWrite == /\ receiverNative /\ receiverOnline /\ acknowledged = 1
                 /\ receiverData' = 2 /\ acknowledged' = 2
                 /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,
                                nextNative,nextData,slot,everInstalled,moving,head,seen,done,receiverOnline>>
FenceDeparture == /\ receiverNative /\ receiverOnline /\ ~moving
                  /\ head \in {"initial","address"}
                  /\ receiverNative' = FALSE /\ moving' = TRUE
                  /\ UNCHANGED <<sourceNative,sourceData,pending,receiverData,
                                 nextNative,nextData,slot,everInstalled,head,seen,done,receiverOnline,acknowledged>>
InstallNext == /\ moving /\ ~nextNative /\ head \in {"initial","address"}
               /\ nextNative' = TRUE /\ nextData' = receiverData
               /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,
                              slot,everInstalled,moving,head,seen,done,receiverOnline,acknowledged>>
PublishDeparture(kind) ==
    /\ kind \in {"moved","retired"} /\ head \in {"initial","address"}
    /\ IF UnsafePublishBeforeDrain THEN pending ELSE (moving /\ nextNative /\ ~slot)
    /\ head' = kind
    /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,
                   nextNative,nextData,slot,everInstalled,moving,seen,done,receiverOnline,acknowledged>>
AddressOnly == /\ head = "initial" /\ receiverOnline
               /\ head' = "address"
               /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,
                              nextNative,nextData,slot,everInstalled,moving,seen,done,receiverOnline,acknowledged>>
Learn == /\ seen # head /\ seen' = head
         /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,
                        nextNative,nextData,slot,everInstalled,moving,head,done,receiverOnline,acknowledged>>
TerminalSeen == seen \in {"moved","retired"} \/ (UnsafeAddressAsDrain /\ seen = "address")
TerminalReceipt == /\ pending /\ ~done /\ TerminalSeen
                   /\ done' = TRUE
                   /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,
                                  nextNative,nextData,slot,everInstalled,moving,head,seen,receiverOnline,acknowledged>>
DirectReceipt == /\ pending /\ ~done /\ everInstalled /\ ~slot /\ receiverOnline
                 /\ done' = TRUE
                 /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,
                                nextNative,nextData,slot,everInstalled,moving,head,seen,receiverOnline,acknowledged>>
CollectSource == /\ done /\ sourceData > 0 /\ sourceData' = 0
                 /\ UNCHANGED <<sourceNative,pending,receiverNative,receiverData,nextNative,nextData,
                                slot,everInstalled,moving,head,seen,done,receiverOnline,acknowledged>>
CollectOldReceiver == /\ head \in {"moved","retired"} /\ nextNative /\ receiverData > 0
                      /\ receiverData' = 0
                      /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,nextNative,nextData,
                                     slot,everInstalled,moving,head,seen,done,receiverOnline,acknowledged>>
(* A return to the original disk is not a terminal receipt by itself. Its
   persistent old closed ticket can still answer a direct retry. Atomic return
   abstracts a separately checked fenced copy/install, not a new consensus. *)
Return == /\ head = "moved" /\ nextNative /\ receiverOnline
          /\ head' = "returned" /\ nextNative' = FALSE
          /\ receiverNative' = TRUE /\ receiverData' = nextData
          /\ UNCHANGED <<sourceNative,sourceData,pending,nextData,slot,everInstalled,
                         moving,seen,done,receiverOnline,acknowledged>>
StopReceiver == /\ receiverOnline /\ receiverOnline' = FALSE
                /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,nextNative,nextData,
                               slot,everInstalled,moving,head,seen,done,acknowledged>>
RestartReceiver == /\ ~receiverOnline /\ receiverOnline' = TRUE
                   /\ UNCHANGED <<sourceNative,sourceData,pending,receiverNative,receiverData,nextNative,nextData,
                                  slot,everInstalled,moving,head,seen,done,acknowledged>>
Next == Emit \/ Install \/ ReceiverWrite \/ FenceDeparture \/ InstallNext
        \/ (\E kind \in {"moved","retired"}: PublishDeparture(kind))
        \/ AddressOnly \/ Learn \/ TerminalReceipt \/ DirectReceipt
        \/ CollectSource \/ CollectOldReceiver \/ Return \/ StopReceiver \/ RestartReceiver
Spec == Init /\ [][Next]_vars
SingleWriter == Cardinality({n \in {"S","R","P"}: CASE n = "S" -> sourceNative
                            [] n = "R" -> receiverNative [] OTHER -> nextNative}) <= 1
SoundReceipt == done => everInstalled
ACKRetained == sourceData >= acknowledged \/ receiverData >= acknowledged \/ nextData >= acknowledged
WriterHasLatest == /\ (sourceNative => sourceData >= acknowledged)
                   /\ (receiverNative => receiverData >= acknowledged)
                   /\ (nextNative => nextData >= acknowledged)
=============================================================================
