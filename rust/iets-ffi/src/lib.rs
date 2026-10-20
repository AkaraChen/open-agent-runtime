//! The C ABI oar calls (include/iets_ffi.h). One evaluation per call, in a
//! session of its own, on a thread with the stack the evaluator needs.
//!
//! Only `iets::api` and the types it hands out are used here, as
//! docs/reference/library.md asks of a frontend.

use std::ffi::{CStr, CString};
use std::io::{self, Write};
use std::os::raw::c_char;
use std::sync::{Arc, Mutex};
use std::thread;

use iets::api::{self, EvalArgs, Input, MAIN_STACK};
use iets::eval::{EvalConfig, Pos};
use iets::session::Session;

/// The session log, kept so the error report can be handed back.
struct Captured(Arc<Mutex<Vec<u8>>>);

impl Write for Captured {
    fn write(&mut self, b: &[u8]) -> io::Result<usize> {
        self.0.lock().unwrap_or_else(|e| e.into_inner()).extend_from_slice(b);
        Ok(b.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn take(log: &Arc<Mutex<Vec<u8>>>) -> Vec<u8> {
    std::mem::take(&mut *log.lock().unwrap_or_else(|e| e.into_inner()))
}

/// What one evaluation produced: the exit code, the JSON, the report.
struct Outcome {
    code: i32,
    json: Vec<u8>,
    err: Vec<u8>,
}

fn evaluate(
    expr: String,
    system: String,
    project_root: Option<String>,
    cache_dir: Option<String>,
    workers: u32,
) -> Outcome {
    let log = Arc::new(Mutex::new(Vec::new()));
    let session = Session::new();
    session.log.write_to(Box::new(Captured(log.clone())));
    let _entered = session.enter();

    let mut args = EvalArgs {
        strict: true,
        json: true,
        input: Some(Input::Expr(expr)),
        no_tracing_cache: true,
        cache_dir,
        ..EvalArgs::new()
    };
    if workers > 0 {
        args.workers = workers as usize;
    }
    // EvalConfig::project_root wants no trailing slash.
    let root = project_root.map(|r| {
        let t = r.trim_end_matches('/');
        if t.is_empty() { b"/".to_vec() } else { t.as_bytes().to_vec() }
    });
    // Run::on, not Run::over: the lints stay off and no NIX_PATH or nix.conf
    // search path is read, which is how iets's classic nix-instantiate
    // behaves and what the builder's expression expects.
    let cfg = EvalConfig::new()
        .system(&system)
        .pure(root.is_some())
        .project_root(root)
        .session(session.clone())
        .search_path(Vec::new())
        .ir_cache(args.ir_cache());
    let run = api::run::Run::on(cfg).workers(args.workers);

    let result = run.go(move |ev, _cache| {
        let v = match api::evaluate(ev, args) {
            Ok(v) => v,
            Err(e) => return Err(api::report(ev, &e)),
        };
        let mut out = Vec::new();
        let mut ctx = Vec::new();
        match iets::print::print_json(ev, v, &mut out, &mut ctx, true, Pos::NONE) {
            Ok(()) => Ok(out),
            Err(e) => Err(api::report(ev, &e)),
        }
    });
    match result {
        Ok(Ok(json)) => Outcome {
            code: 0,
            json,
            err: take(&log),
        },
        Ok(Err(code)) => Outcome {
            code: if code == 0 { 1 } else { code },
            json: Vec::new(),
            err: take(&log),
        },
        Err(e) => Outcome {
            code: 1,
            json: Vec::new(),
            err: format!("error: {e}\n").into_bytes(),
        },
    }
}

/// SAFETY: `p` is NULL or a NUL-terminated string the caller keeps alive
/// for the call.
unsafe fn arg(p: *const c_char) -> Option<String> {
    if p.is_null() {
        return None;
    }
    // SAFETY: as above.
    Some(unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned())
}

fn hand_out(bytes: Vec<u8>) -> *mut c_char {
    if bytes.is_empty() {
        return std::ptr::null_mut();
    }
    // A NUL inside would truncate; the JSON printer and the report never
    // write one, and a value that did is an error worth seeing.
    let s = String::from_utf8_lossy(&bytes).replace('\0', "\u{fffd}");
    CString::new(s).map_or(std::ptr::null_mut(), CString::into_raw)
}

/// # Safety
/// Pointers follow include/iets_ffi.h.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn oar_iets_eval(
    expr: *const c_char,
    system: *const c_char,
    project_root: *const c_char,
    cache_dir: *const c_char,
    workers: u32,
    json_out: *mut *mut c_char,
    error_out: *mut *mut c_char,
) -> i32 {
    // SAFETY: the header says the out pointers are valid.
    unsafe {
        if !json_out.is_null() {
            *json_out = std::ptr::null_mut();
        }
        if !error_out.is_null() {
            *error_out = std::ptr::null_mut();
        }
    }
    // SAFETY: the header says the inputs are valid C strings or NULL.
    let (expr, system, root, cache) = unsafe {
        (arg(expr), arg(system), arg(project_root), arg(cache_dir))
    };
    let (Some(expr), Some(system)) = (expr, system) else {
        // SAFETY: as above.
        unsafe {
            if !error_out.is_null() {
                *error_out = hand_out(b"error: expr and system are required\n".to_vec());
            }
        }
        return 2;
    };

    // With one worker the evaluator runs on the calling thread's stack, so
    // give it the stack the iets command line uses. The thread is joined
    // before this returns.
    let joined = thread::Builder::new()
        .name("iets-eval".into())
        .stack_size(MAIN_STACK)
        .spawn(move || evaluate(expr, system, root, cache, workers))
        .map(|h| h.join());
    let out = match joined {
        Ok(Ok(o)) => o,
        Ok(Err(_)) => Outcome {
            code: 1,
            json: Vec::new(),
            err: b"error: the evaluator thread panicked\n".to_vec(),
        },
        Err(e) => Outcome {
            code: 1,
            json: Vec::new(),
            err: format!("error: cannot start the evaluator thread: {e}\n").into_bytes(),
        },
    };
    // SAFETY: as above.
    unsafe {
        if !json_out.is_null() {
            *json_out = hand_out(out.json);
        }
        if !error_out.is_null() {
            *error_out = hand_out(out.err);
        }
    }
    out.code
}

/// # Safety
/// `p` came from oar_iets_eval, or is NULL.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn oar_iets_free(p: *mut c_char) {
    if !p.is_null() {
        // SAFETY: made by CString::into_raw in hand_out.
        drop(unsafe { CString::from_raw(p) });
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn oar_iets_version() -> *const c_char {
    static VERSION: &[u8] = b"iets 0.1.0 (bdd26f2c)\0";
    VERSION.as_ptr() as *const c_char
}
