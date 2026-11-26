CREATE ROLE stress_target LOGIN PASSWORD 'if3110-stress-target';
CREATE DATABASE stress_a OWNER stress_target;
CREATE DATABASE stress_b OWNER stress_target;

\connect stress_a
SET ROLE stress_target;
CREATE TABLE public.k6_counters (id bigint PRIMARY KEY, value bigint NOT NULL);
INSERT INTO public.k6_counters VALUES (1, 0);
RESET ROLE;

\connect stress_b
SET ROLE stress_target;
CREATE TABLE public.k6_counters (id bigint PRIMARY KEY, value bigint NOT NULL);
INSERT INTO public.k6_counters VALUES (1, 0);
RESET ROLE;
