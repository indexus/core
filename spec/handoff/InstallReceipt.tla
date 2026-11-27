------------------------- MODULE InstallReceipt -------------------------
EXTENDS TLC
CONSTANT RequireLiveToken
VARIABLES emitted, installed, live, payload, context, ack, done
vars == <<emitted, installed, live, payload, context, ack, done>>
Init == /\ emitted = FALSE /\ installed = FALSE /\ live = FALSE
        /\ payload = FALSE /\ context = FALSE /\ ack = FALSE /\ done = FALSE
Emit == /\ ~emitted /\ emitted' = TRUE
        /\ UNCHANGED <<installed, live, payload, context, ack, done>>
Install == /\ emitted /\ ~installed
           /\ installed' = TRUE /\ live' = TRUE
           /\ payload' = TRUE
           /\ UNCHANGED <<emitted, context, ack, done>>
Coalesce == /\ installed /\ live /\ live' = FALSE
            /\ context' = TRUE
            /\ UNCHANGED <<emitted, installed, payload, ack, done>>
Compact == /\ installed /\ ~live /\ payload /\ payload' = FALSE
           /\ UNCHANGED <<emitted, installed, live, context, ack, done>>
Reply == /\ installed /\ ~ack
         /\ (live \/ payload \/ (~RequireLiveToken /\ context))
         /\ ack' = TRUE
         /\ UNCHANGED <<emitted, installed, live, payload, context, done>>
Finish == /\ ack /\ ~done /\ done' = TRUE
          /\ UNCHANGED <<emitted, installed, live, payload, context, ack>>
Next == Emit \/ Install \/ Coalesce \/ Compact \/ Reply \/ Finish
SingleWriter == ~(~emitted /\ installed)
SoundReceipt == context => installed
EventuallyClosed == <>done
Spec == Init /\ [][Next]_vars /\ WF_vars(Emit) /\ WF_vars(Install)
        /\ WF_vars(Coalesce) /\ WF_vars(Compact) /\ WF_vars(Reply) /\ WF_vars(Finish)
=============================================================================
