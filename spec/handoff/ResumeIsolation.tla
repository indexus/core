------------------------- MODULE ResumeIsolation -------------------------
EXTENDS Naturals
CONSTANT Coupled
VARIABLE phase
vars == <<phase>>
(* A -> B is unavailable forever. B -> C was already issued; C is healthy.
   We deliberately do not assume A recovers to prove independent progress.
   Stages abstract install, source confirmation and local collection. *)
Init == phase = 0
Progress == /\ phase < 3
            /\ ~Coupled
            /\ phase' = phase + 1
Next == Progress
Spec == Init /\ [][Next]_vars /\ WF_vars(Progress)
TypeOK == phase \in 0..3
OutgoingCloses == <>(phase = 3)
=============================================================================
