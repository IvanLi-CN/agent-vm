#!/usr/bin/env bun
/** Discover and run maintained TS, Python, and Shell tests without a root edit per sibling. */

import { dirname, join, relative } from "node:path";
import { isDirectRegularFile, isPathInside, readText, REPO_ROOT, relativePath, walkFiles } from "./repo_util.ts";

export type TestScope = "repo" | "skills";
export type TestKind = "bun" | "python" | "shell";
export type TestFile = { path: string; kind: TestKind };

const IGNORED_DIRECTORIES = new Set([
  "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", "dist", "build", "coverage", "reports", "artifacts", "tmp",
  ".git",
]);

const PYTHON_TEST_RUNNER = [
  "import importlib.util",
  "import functools",
  "import inspect",
  "import pathlib",
  "import sys",
  "import unittest",
  "import unittest.case",
  "path = pathlib.Path(sys.argv[1])",
  "sys.path.insert(0, str(path.parent))",
  "spec = importlib.util.spec_from_file_location('agent_toolkit_test_module', path)",
  "if spec is None or spec.loader is None:",
  "    raise ImportError(path)",
  "module = importlib.util.module_from_spec(spec)",
  "sys.modules[spec.name] = module",
  "unsupported_results = []",
  "def reject_unsupported_result(result):",
  "    if inspect.isawaitable(result) or inspect.isasyncgen(result) or inspect.isgenerator(result):",
  "        unsupported_results.append(True)",
  "        raise TypeError('async or generator tests are unsupported')",
  "    return result",
  "def refresh_runtime_guards():",
  "    pass",
  "def guard_cleanup_callback(original):",
  "    if getattr(original, '_agent_toolkit_cleanup_guarded', False):",
  "        return original",
  "    def guarded(*args, **kwargs):",
  "        try:",
  "            return reject_unsupported_result(original(*args, **kwargs))",
  "        finally:",
  "            refresh_runtime_guards()",
  "    setattr(guarded, '_agent_toolkit_cleanup_guarded', True)",
  "    return guarded",
  "original_add_module_cleanup = unittest.addModuleCleanup",
  "def guarded_add_module_cleanup(function, /, *args, **kwargs):",
  "    return reject_unsupported_result(original_add_module_cleanup(guard_cleanup_callback(function), *args, **kwargs))",
  "setattr(guarded_add_module_cleanup, '_agent_toolkit_cleanup_registration_guarded', True)",
  "unittest.addModuleCleanup = guarded_add_module_cleanup",
  "unittest.case.addModuleCleanup = guarded_add_module_cleanup",
  "original_add_class_cleanup = unittest.TestCase.addClassCleanup.__func__",
  "def guarded_add_class_cleanup(test_class, function, /, *args, **kwargs):",
  "    return reject_unsupported_result(original_add_class_cleanup(test_class, guard_cleanup_callback(function), *args, **kwargs))",
  "unittest.TestCase.addClassCleanup = classmethod(guarded_add_class_cleanup)",
  "original_add_instance_cleanup = unittest.TestCase.addCleanup",
  "def guarded_add_instance_cleanup(case, function, /, *args, **kwargs):",
  "    return reject_unsupported_result(original_add_instance_cleanup(case, guard_cleanup_callback(function), *args, **kwargs))",
  "unittest.TestCase.addCleanup = guarded_add_instance_cleanup",
  "try:",
  "    spec.loader.exec_module(module)",
  "except SystemExit as error:",
  "    raise RuntimeError(f'test module exited during import: {error.code!r}') from error",
  "def unwrapped(value):",
  "    try:",
  "        return inspect.unwrap(value)",
  "    except (TypeError, ValueError):",
  "        return value",
  "load_tests_hook = getattr(module, 'load_tests', None)",
  "async_names = []",
  "try:",
  "    suite = unittest.defaultTestLoader.loadTestsFromModule(module)",
  "except SystemExit as error:",
  "    raise RuntimeError(f'test module exited during test discovery: {error.code!r}') from error",
  "def async_test_names(test_suite):",
  "    names = []",
  "    classes = set()",
  "    modules = set()",
  "    for case in test_suite:",
  "        if isinstance(case, unittest.BaseTestSuite):",
  "            names.extend(async_test_names(case))",
  "        elif isinstance(case, unittest.TestCase):",
  "            classes.add(case.__class__)",
  "            modules.add(case.__class__.__module__)",
  "            if isinstance(case, unittest.FunctionTestCase):",
  "                for attribute in ('_testFunc', '_setUpFunc', '_tearDownFunc'):",
  "                    function = unwrapped(getattr(case, attribute, None))",
  "                    if function is not None and (inspect.iscoroutinefunction(function) or inspect.isasyncgenfunction(function) or inspect.isgeneratorfunction(function)):",
  "                        names.append(f'{case.id()}.{attribute}')",
  "                continue",
  "            method_name = getattr(case, '_testMethodName', '')",
  "            method = unwrapped(getattr(case, method_name, None))",
  "            if method is not None and (inspect.iscoroutinefunction(method) or inspect.isasyncgenfunction(method) or inspect.isgeneratorfunction(method)):",
  "                names.append(f'{case.__class__.__name__}.{method_name}')",
  "    for test_class in classes:",
  "        for hook in ('setUp', 'tearDown', 'setUpClass', 'tearDownClass'):",
  "            method = unwrapped(getattr(test_class, hook, None))",
  "            if method is not None and (inspect.iscoroutinefunction(method) or inspect.isasyncgenfunction(method) or inspect.isgeneratorfunction(method)):",
  "                names.append(f'{test_class.__module__}.{test_class.__name__}.{hook}')",
  "    for module_name in modules:",
  "        target_module = sys.modules.get(module_name)",
  "        for hook in ('setUpModule', 'tearDownModule'):",
  "            method = unwrapped(getattr(target_module, hook, None)) if target_module is not None else None",
  "            if method is not None and (inspect.iscoroutinefunction(method) or inspect.isasyncgenfunction(method) or inspect.isgeneratorfunction(method)):",
  "                names.append(f'{module_name}.{hook}')",
  "    return names",
  "async_names += async_test_names(suite)",
  "if not callable(load_tests_hook):",
  "    async_names += [name for name, value in vars(module).items() if name.startswith('test_') and (inspect.iscoroutinefunction(unwrapped(value)) or inspect.isasyncgenfunction(unwrapped(value)) or inspect.isgeneratorfunction(unwrapped(value)))]",
  "if async_names:",
  "    raise TypeError('async tests are unsupported: ' + ', '.join(async_names))",
  "def preserve_metadata(original, guarded):",
  "    try:",
  "        return functools.update_wrapper(guarded, original)",
  "    except (AttributeError, TypeError):",
  "        return guarded",
  "def guard_result_function(original, refresh=None, target_getter=None):",
  "    if getattr(original, '_agent_toolkit_guarded_function', False):",
  "        return original",
  "    def guarded(*args, **kwargs):",
  "        target = original",
  "        if target_getter is not None:",
  "            candidate = target_getter()",
  "            if candidate is not guarded:",
  "                if refresh is not None:",
  "                    refresh()",
  "                    candidate = target_getter()",
  "                if candidate is not guarded and candidate is not None:",
  "                    target = candidate",
  "        try:",
  "            return reject_unsupported_result(target(*args, **kwargs))",
  "        finally:",
  "            if refresh is not None:",
  "                refresh()",
  "    guarded = preserve_metadata(original, guarded)",
  "    setattr(guarded, '_agent_toolkit_guarded_function', True)",
  "    return guarded",
  "guarded_cases_by_class = {}",
  "guarded_function_cases = []",
  "def same_method_target(left, right):",
  "    return left is right or (getattr(left, '__self__', None) is getattr(right, '__self__', None) and getattr(left, '__func__', left) is getattr(right, '__func__', right))",
  "def sync_method_metadata(guarded, target):",
  "    for metadata in ('__unittest_expecting_failure__', '__unittest_skip__', '__unittest_skip_why__'):",
  "        if hasattr(target, metadata):",
  "            setattr(guarded, metadata, getattr(target, metadata))",
  "        else:",
  "            try:",
  "                delattr(guarded, metadata)",
  "            except AttributeError:",
  "                pass",
  "def refresh_instance_method_metadata(test_class):",
  "    for case in guarded_cases_by_class.get(test_class, ()):",
  "        for method_name, guarded in list(case.__dict__.items()):",
  "            if not getattr(guarded, '_agent_toolkit_guarded_method', False):",
  "                continue",
  "            class_target = getattr(test_class, method_name, None)",
  "            if class_target is not None and class_target is not guarded:",
  "                if getattr(guarded, '_agent_toolkit_initial_instance_target', None) is not None:",
  "                    continue",
  "                initial_class_target = getattr(guarded, '_agent_toolkit_initial_class_target', None)",
  "                if method_name == getattr(case, '_testMethodName', None) and getattr(guarded, '__unittest_expecting_failure__', False) and initial_class_target is not None and not same_method_target(class_target, initial_class_target):",
  "                    raise AssertionError('test method was replaced on its class; expected-failure metadata is no longer reliable')",
  "                sync_method_metadata(guarded, class_target)",
  "def refresh_test_method_after_setup(case):",
  "    test_method_name = getattr(case, '_testMethodName', '')",
  "    guarded = getattr(case, '_agent_toolkit_test_method_guard', None)",
  "    replacement = case.__dict__.get(test_method_name)",
  "    if not getattr(guarded, '_agent_toolkit_guarded_method', False) or replacement is None or replacement is guarded:",
  "        return",
  "    if getattr(guarded, '__unittest_expecting_failure__', False):",
  "        raise AssertionError('test method was replaced during setUp; expected-failure metadata is no longer reliable')",
  "    sync_method_metadata(guarded, replacement)",
  "def guard_module_test_classes(module):",
  "    for test_class in list(guarded_cases_by_class):",
  "        if test_class.__module__ != module.__name__:",
  "            continue",
  "        guard_class_method(test_class, 'setUpClass')",
  "        guard_class_method(test_class, 'tearDownClass')",
  "        guard_class_cleanup(test_class)",
  "        refresh_instance_method_metadata(test_class)",
  "def guard_module_method(module, method_name):",
  "    existing = getattr(module, method_name, None)",
  "    if getattr(existing, '_agent_toolkit_guarded_method', False):",
  "        return",
  "    original = existing",
  "    if original is None:",
  "        return",
  "    def guarded(*args, **kwargs):",
  "        try:",
  "            return reject_unsupported_result(original(*args, **kwargs))",
  "        finally:",
  "            refresh_runtime_guards()",
  "    guarded = preserve_metadata(original, guarded)",
  "    setattr(guarded, '_agent_toolkit_guarded_method', True)",
  "    setattr(module, method_name, guarded)",
  "guard_module_method(module, 'setUpModule')",
  "guard_module_method(module, 'tearDownModule')",
  "def guard_module_cleanup_registration(target_module):",
  "    existing = getattr(target_module, 'addModuleCleanup', None)",
  "    if existing is not None and not getattr(existing, '_agent_toolkit_cleanup_registration_guarded', False):",
  "        def guarded(function, /, *args, **kwargs):",
  "            return reject_unsupported_result(existing(guard_cleanup_callback(function), *args, **kwargs))",
  "        setattr(guarded, '_agent_toolkit_cleanup_registration_guarded', True)",
  "        setattr(target_module, 'addModuleCleanup', guarded)",
  "    cleanups = getattr(target_module, '_module_cleanups', None)",
  "    if isinstance(cleanups, list):",
  "        for index, (function, args, kwargs) in enumerate(cleanups):",
  "            cleanups[index] = (guard_cleanup_callback(function), args, kwargs)",
  "def guard_instance_method(case, method_name):",
  "    existing = case.__dict__.get(method_name)",
  "    if getattr(existing, '_agent_toolkit_guarded_method', False):",
  "        return",
  "    original = getattr(case, method_name, None)",
  "    if original is None:",
  "        return",
  "    initial_instance_method = case.__dict__.get(method_name)",
  "    initial_target = original",
  "    initial_class_target = getattr(case.__class__, method_name, None)",
  "    def guarded(*args, **kwargs):",
  "        replacement = case.__dict__.get(method_name)",
  "        case.__dict__.pop(method_name, None)",
  "        try:",
  "            target = replacement if replacement is not None and replacement is not guarded else (initial_instance_method if initial_instance_method is not None else getattr(case, method_name, None))",
  "            if target is None:",
  "                target = initial_target",
  "            sync_method_metadata(guarded, target)",
  "            return reject_unsupported_result(target(*args, **kwargs))",
  "        finally:",
  "            current = case.__dict__.get(method_name)",
  "            if current is None or current is guarded:",
  "                case.__dict__[method_name] = guarded",
  "            refresh_runtime_guards()",
  "    guarded = preserve_metadata(original, guarded)",
  "    setattr(guarded, '_agent_toolkit_guarded_method', True)",
  "    setattr(case, method_name, guarded)",
  "    setattr(guarded, '_agent_toolkit_initial_instance_target', initial_instance_method)",
  "    if method_name == getattr(case, '_testMethodName', None):",
  "        setattr(case, '_agent_toolkit_test_method_guard', guarded)",
  "        setattr(guarded, '_agent_toolkit_initial_class_target', initial_class_target)",
  "    cases = guarded_cases_by_class.setdefault(case.__class__, [])",
  "    if not any(existing is case for existing in cases):",
  "        cases.append(case)",
  "def guard_function_test_case(case):",
  "    if not any(existing is case for existing in guarded_function_cases):",
  "        guarded_function_cases.append(case)",
  "    for attribute in ('_testFunc', '_setUpFunc', '_tearDownFunc'):",
  "        original = getattr(case, attribute, None)",
  "        if original is None or getattr(original, '_agent_toolkit_guarded_function', False):",
  "            continue",
  "        if attribute == '_testFunc':",
  "            initial = case.__dict__.setdefault('_agent_toolkit_initial_test_func', original)",
  "            if original is not initial and getattr(case, '__unittest_expecting_failure__', False):",
  "                raise AssertionError('FunctionTestCase test function was replaced; expected-failure metadata is no longer reliable')",
  "        setattr(case, attribute, guard_result_function(original, lambda: refresh_runtime_guards(), lambda attribute=attribute: getattr(case, attribute, None)))",
  "        if attribute == '_testFunc':",
  "            for metadata in ('__unittest_expecting_failure__', '__unittest_skip__', '__unittest_skip_why__'):",
  "                if hasattr(original, metadata):",
  "                    setattr(case, metadata, getattr(original, metadata))",
  "def refresh_function_cases():",
  "    for case in list(guarded_function_cases):",
  "        guard_function_test_case(case)",
  "def guard_instance_cleanup(case):",
  "    original = getattr(case, 'addCleanup', None)",
  "    if original is not None and not getattr(original, '_agent_toolkit_cleanup_method_guarded', False):",
  "        def guarded(function, /, *args, **kwargs):",
  "            return reject_unsupported_result(original(guard_cleanup_callback(function), *args, **kwargs))",
  "        setattr(guarded, '_agent_toolkit_cleanup_method_guarded', True)",
  "        setattr(case, 'addCleanup', guarded)",
  "    cleanups = getattr(case, '_cleanups', None)",
  "    if isinstance(cleanups, list):",
  "        for index, (function, args, kwargs) in enumerate(cleanups):",
  "            cleanups[index] = (guard_cleanup_callback(function), args, kwargs)",
  "def guard_class_cleanup(test_class):",
  "    cleanups = getattr(test_class, '_class_cleanups', None)",
  "    if isinstance(cleanups, list):",
  "        for index, (function, args, kwargs) in enumerate(cleanups):",
  "            cleanups[index] = (guard_cleanup_callback(function), args, kwargs)",
  "    current = inspect.getattr_static(test_class, 'addClassCleanup', None)",
  "    previous = test_class.__dict__.get('_agent_toolkit_class_cleanup_wrapper')",
  "    if previous is not None and getattr(current, '__func__', current) is previous:",
  "        return",
  "    descriptor = current",
  "    original = getattr(test_class, 'addClassCleanup', None)",
  "    if original is None:",
  "        return",
  "    if getattr(original, '_agent_toolkit_cleanup_method_guarded', False):",
  "        return",
  "    if isinstance(descriptor, staticmethod):",
  "        def guarded(function, /, *args, **kwargs):",
  "            return reject_unsupported_result(original(guard_cleanup_callback(function), *args, **kwargs))",
  "        setattr(test_class, 'addClassCleanup', staticmethod(preserve_metadata(original, guarded)))",
  "    elif isinstance(descriptor, classmethod):",
  "        original_function = descriptor.__func__",
  "        def guarded(receiver, function, /, *args, **kwargs):",
  "            return reject_unsupported_result(original_function(receiver, guard_cleanup_callback(function), *args, **kwargs))",
  "        setattr(test_class, 'addClassCleanup', classmethod(preserve_metadata(original_function, guarded)))",
  "    elif inspect.ismethod(original) and not inspect.isfunction(descriptor):",
  "        def guarded(function, /, *args, **kwargs):",
  "            return reject_unsupported_result(original(guard_cleanup_callback(function), *args, **kwargs))",
  "        setattr(test_class, 'addClassCleanup', staticmethod(preserve_metadata(original, guarded)))",
    "    elif inspect.isfunction(descriptor):",
    "        original_function = descriptor",
    "        def guarded(receiver, function, /, *args, **kwargs):",
    "            return reject_unsupported_result(original_function(receiver, guard_cleanup_callback(function), *args, **kwargs))",
    "        setattr(test_class, 'addClassCleanup', preserve_metadata(original_function, guarded))",
    "    else:",
  "        original_function = descriptor if descriptor is not None else original",
  "        def guarded(function, /, *args, **kwargs):",
  "            return reject_unsupported_result(original_function(guard_cleanup_callback(function), *args, **kwargs))",
  "        setattr(test_class, 'addClassCleanup', preserve_metadata(original_function, guarded))",
  "    current = inspect.getattr_static(test_class, 'addClassCleanup', None)",
  "    setattr(test_class, '_agent_toolkit_class_cleanup_wrapper', getattr(current, '__func__', current))",
  "    setattr(test_class, '_agent_toolkit_class_cleanup_guarded', True)",
  "def guard_class_method(test_class, method_name):",
  "    descriptor = inspect.getattr_static(test_class, method_name, None)",
  "    original = getattr(test_class, method_name, None)",
  "    if original is None:",
  "        return",
  "    original_function = getattr(descriptor, '__func__', descriptor)",
  "    if getattr(original_function, '_agent_toolkit_guarded_method', False):",
  "        return",
  "    if isinstance(descriptor, classmethod):",
  "        def guarded(receiver):",
  "            try:",
  "                return reject_unsupported_result(original_function(receiver))",
  "            finally:",
  "                refresh_runtime_guards()",
  "    elif isinstance(descriptor, staticmethod):",
  "        def guarded(receiver):",
  "            try:",
  "                return reject_unsupported_result(original_function())",
  "            finally:",
  "                refresh_runtime_guards()",
  "    elif inspect.ismethod(original) and not inspect.isfunction(descriptor):",
  "        def guarded(receiver):",
  "            try:",
  "                return reject_unsupported_result(original())",
  "            finally:",
  "                refresh_runtime_guards()",
  "    else:",
  "        def guarded(receiver):",
  "            try:",
  "                return reject_unsupported_result(original())",
  "            finally:",
  "                refresh_runtime_guards()",
  "    guarded = preserve_metadata(original_function, guarded)",
  "    setattr(guarded, '_agent_toolkit_guarded_method', True)",
  "    setattr(test_class, method_name, classmethod(guarded))",
  "def guard_test_cases(test_suite):",
  "    classes = set()",
  "    for case in test_suite:",
  "        if isinstance(case, unittest.BaseTestSuite):",
  "            guard_test_cases(case)",
  "        elif isinstance(case, unittest.TestCase):",
  "            classes.add(case.__class__)",
  "            if isinstance(case, unittest.FunctionTestCase):",
  "                guard_function_test_case(case)",
  "            guard_instance_cleanup(case)",
  "            method_name = getattr(case, '_testMethodName', '')",
  "            guard_instance_method(case, method_name)",
  "            guard_instance_method(case, 'setUp')",
  "            guard_instance_method(case, 'tearDown')",
  "            guard_instance_method(case, 'doCleanups')",
  "    for test_class in classes:",
  "        guard_class_cleanup(test_class)",
  "        guard_class_method(test_class, 'setUpClass')",
  "        guard_class_method(test_class, 'tearDownClass')",
  "        guard_class_method(test_class, 'doClassCleanups')",
  "guard_test_cases(suite)",
  "def suite_modules(test_suite):",
  "    modules = set()",
  "    for case in test_suite:",
  "        if isinstance(case, unittest.BaseTestSuite):",
  "            modules.update(suite_modules(case))",
  "        elif isinstance(case, unittest.TestCase):",
  "            modules.add(case.__class__.__module__)",
  "    return modules",
  "guarded_module_names = {module.__name__}",
  "guarded_module_names.update(suite_modules(suite))",
  "def refresh_runtime_guards():",
  "    guard_module_cleanup_registration(unittest)",
  "    guard_module_cleanup_registration(unittest.case)",
  "    guard_module_method(unittest, 'doModuleCleanups')",
  "    guard_module_method(unittest.case, 'doModuleCleanups')",
  "    refresh_function_cases()",
  "    for test_class, cases in list(guarded_cases_by_class.items()):",
  "        for case in list(cases):",
  "            refresh_test_method_after_setup(case)",
  "            guard_instance_method(case, getattr(case, '_testMethodName', ''))",
  "            guard_instance_method(case, 'setUp')",
  "            guard_instance_method(case, 'tearDown')",
  "            guard_instance_method(case, 'doCleanups')",
  "            guard_instance_cleanup(case)",
  "        guard_class_method(test_class, 'setUpClass')",
  "        guard_class_method(test_class, 'tearDownClass')",
  "        guard_class_method(test_class, 'doClassCleanups')",
  "        guard_class_cleanup(test_class)",
  "        refresh_instance_method_metadata(test_class)",
  "    for module_name in guarded_module_names:",
  "        target_module = sys.modules.get(module_name)",
  "        if target_module is not None:",
  "            guard_module_method(target_module, 'setUpModule')",
  "            guard_module_method(target_module, 'tearDownModule')",
  "guard_module_cleanup_registration(unittest)",
  "guard_module_cleanup_registration(unittest.case)",
  "guard_module_method(unittest, 'doModuleCleanups')",
  "guard_module_method(unittest.case, 'doModuleCleanups')",
  "for module_name in guarded_module_names:",
  "    target_module = sys.modules.get(module_name)",
  "    if target_module is not None:",
  "        guard_module_method(target_module, 'setUpModule')",
  "        guard_module_method(target_module, 'tearDownModule')",
  "def run_module_test(value):",
  "    def run():",
  "        try:",
  "            result = value()",
  "            reject_unsupported_result(result)",
  "        finally:",
  "            refresh_runtime_guards()",
  "    return preserve_metadata(value, run)",
  "def module_function_case(value):",
  "    test_function = run_module_test(value)",
  "    metadata = {'__module__': module.__name__}",
  "    for name in ('__unittest_expecting_failure__', '__unittest_skip__', '__unittest_skip_why__'):",
  "        if hasattr(test_function, name):",
  "            metadata[name] = getattr(test_function, name)",
  "    test_case_type = type('ModuleFunctionTestCase', (unittest.FunctionTestCase,), metadata)",
  "    return test_case_type(test_function)",
  "if not callable(load_tests_hook):",
  "    suite.addTests(module_function_case(value) for name, value in vars(module).items() if name.startswith('test_') and callable(value))",
  "    guard_test_cases(suite)",
  "result = unittest.TextTestRunner(verbosity=1).run(suite)",
  "if result.testsRun == 0:",
  "    print('ERROR no Python tests executed', file=sys.stderr)",
  "    raise SystemExit(1)",
  "if result.testsRun == len(result.skipped):",
  "    print('ERROR no non-skipped Python tests executed', file=sys.stderr)",
  "    raise SystemExit(1)",
  "if unsupported_results:",
  "    raise SystemExit(1)",
  "raise SystemExit(0 if result.wasSuccessful() else 1)",
].join("\n");

export function testKind(path: string): TestKind | undefined {
  const name = path.split(/[\\/]/).at(-1) ?? "";
  if (name.endsWith(".test.ts") || /^test[-_].*\.ts$/.test(name) || /_test\.ts$/.test(name)) return "bun";
  if (name.endsWith(".test.py") || /^test[-_].*\.py$/.test(name) || /_test\.py$/.test(name)) return "python";
  if (name.endsWith(".test.sh") || /^test[-_].*\.sh$/.test(name) || /_test\.sh$/.test(name)) return "shell";
  return undefined;
}

export function discoverTests(root: string = REPO_ROOT, scope: TestScope = "repo"): TestFile[] {
  const skillsRoot = join(root, "skills");
  const rejectedSymlinks: string[] = [];
  const files = walkFiles(root, {
    ignoredDirectories: IGNORED_DIRECTORIES,
    shouldIgnoreDirectory: (path, name) => {
      const isDirectSkill = path !== skillsRoot && dirname(path) === skillsRoot;
      return !isDirectSkill && (IGNORED_DIRECTORIES.has(name) || name.endsWith(".egg-info"));
    },
    onRejectedSymlink: (path) => rejectedSymlinks.push(path),
  });
  if (rejectedSymlinks.length > 0) {
    throw new Error(`refusing to discover tests through rejected symlink paths: ${rejectedSymlinks.sort().join(", ")}`);
  }
  return files
    .filter((path) => path !== join(root, "bin", "test_runner.ts"))
    .filter((path) => (scope === "repo" ? !path.startsWith(join(root, "skills") + "/") && !path.startsWith(join(root, "skills") + "\\") : path.startsWith(join(root, "skills") + "/") || path.startsWith(join(root, "skills") + "\\")))
    .flatMap((path) => {
      const kind = testKind(path);
      return kind ? [{ path, kind }] : [];
    })
    .sort((left, right) => (left.path < right.path ? -1 : left.path > right.path ? 1 : 0));
}

export function ensureTestSelection(files: TestFile[], scope: TestScope): void {
  if (files.length === 0) {
    throw new Error(`no maintained ${scope} tests were discovered; refusing to pass an empty test selection`);
  }
}

export function ensureTestSources(files: TestFile[]): void {
  const invalid = files.filter((test) => {
    const source = readText(test.path);
    if (source.trim().length === 0) return true;
    if (test.kind === "bun") return !/\b(?:test|it|describe)(?:\s*\.\s*[A-Za-z_$][\w$]*)*\s*\(/.test(source);
    if (test.kind === "python") return !/\b(?:unittest\.|TestCase\b|def\s+test_)/.test(source);
    const hasTestCommand = source.split("\n").some((line) => {
      const executableLine = line
        .replace(/'(?:[^'\\]|\\.)*'/g, "''")
        .replace(/"(?:[^"\\]|\\.)*"/g, "\"\"")
        .replace(/\s+#.*$/, "");
      const commandBoundary = /(?:^|[;&|(){}]\s*|\b(?:then|do)\s+)(?:if\s+)?!?\s*(?:[A-Za-z_][A-Za-z0-9_]*=[^\s;&|(){}]+\s*)*/;
      return new RegExp(`${commandBoundary.source}(?:(?:builtin|command)(?:\\s+(?:--|-p))*\\s+)?(?:["']?test["']?)(?:\\s|$|[;&|(){}])`).test(executableLine);
    });
    return !hasTestCommand;
  });
  if (invalid.length > 0) {
    throw new Error(`refusing to run test files without maintained test declarations: ${invalid.map((test) => test.path).join(", ")}`);
  }
}

export function executedTestCount(kind: TestKind, output: string): number | undefined {
  const summary = kind === "shell"
    ? /^\s*TESTS_RUN\s*=\s*(\d+)\s*$/gim
    : kind === "bun"
      ? /^\s*Ran\s+(\d+)\s+tests?\s+across\b/gim
      : /^\s*Ran\s+(\d+)\s+tests?\s+in\b/gim;
  const matches = [...output.matchAll(summary)];
  if (matches.length !== 1) return undefined;
  return Number(matches[0]?.[1]);
}

function hasExecutedTests(kind: TestKind, output: string, count: number | undefined): boolean {
  if (count === undefined || count === 0) return false;
  if (kind !== "bun") return true;
  const passSummaries = [...output.matchAll(/^\s*(\d+)\s+pass(?:es)?\s*$/gim)];
  return passSummaries.length === 1 && Number(passSummaries[0]?.[1]) > 0;
}

export function commandForTest(test: TestFile): string[] {
  if (test.kind === "bun") return [process.execPath, "test", test.path];
  if (test.kind === "python") return ["python3", "-c", PYTHON_TEST_RUNNER, test.path];
  const shellCommand = [
    "set -euo pipefail",
    "set -m",
    String.raw`
__agent_toolkit_stdout_file=""
__agent_toolkit_stderr_file=""
marker_file=""
__agent_toolkit_stdout_replay_pid=""
__agent_toolkit_stderr_replay_pid=""
__agent_toolkit_source_pid=""
__agent_toolkit_cleanup_done=0
__agent_toolkit_replay_done=0
__agent_toolkit_signal_group() {
  local signal="$1" pid="$2"
  [[ -n "$pid" ]] || return 0
  if ! builtin kill "-$signal" "-$pid" 2>/dev/null; then
    builtin kill "-$signal" "$pid" 2>/dev/null || true
  fi
}
__agent_toolkit_stop_group() {
  local pid="$1" attempt=0
  [[ -n "$pid" ]] || return 0
  __agent_toolkit_signal_group TERM "$pid"
  while (( attempt < 50 )); do
    builtin kill -0 "-$pid" 2>/dev/null || return 0
    sleep 0.01
    ((attempt += 1))
  done
  __agent_toolkit_signal_group KILL "$pid"
}
__agent_toolkit_process_active() {
  local pid="$1" state
  [[ -n "$pid" ]] || return 1
  state="$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')"
  [[ -n "$state" && "$state" != Z* ]]
}
__agent_toolkit_kill_owned_session_groups() {
  local session_id signal group
  session_id="$(__agent_toolkit_session_id)"
  [[ "$session_id" == "$$" ]] || return 0
  for signal in TERM KILL; do
    while read -r group; do
      [[ -n "$group" && "$group" != "$$" ]] || continue
      builtin kill "-$signal" "-$group" 2>/dev/null || true
    done < <(__agent_toolkit_session_groups "$session_id")
    [[ "$signal" == TERM ]] && sleep 0.05
  done
}
__agent_toolkit_session_id() {
  local session_id
  session_id="$(ps -o sess= -p "$$" 2>/dev/null | tr -d ' ')" || session_id=""
  if [[ -z "$session_id" || "$session_id" == 0 ]]; then
    session_id="$(ps -o sid= -p "$$" 2>/dev/null | tr -d ' ')" || session_id=""
  fi
  printf '%s' "$session_id"
}
__agent_toolkit_session_groups() {
  local session_id="$1" table
  table="$(ps -axo pid=,sess=,pgid= 2>/dev/null)" || table=""
  if [[ -z "$table" ]]; then
    table="$(ps -axo pid=,sid=,pgid= 2>/dev/null)" || table=""
  fi
  awk -v session="$session_id" '$2 == session {print $3}' <<<"$table" | sort -u
}
__agent_toolkit_relay_drained() {
  ! __agent_toolkit_process_active "$__agent_toolkit_stdout_replay_pid" &&
    ! __agent_toolkit_process_active "$__agent_toolkit_stderr_replay_pid"
}
__agent_toolkit_wait_for_relays() {
  local attempt=0
  while ! __agent_toolkit_relay_drained && (( attempt < 100 )); do
    sleep 0.01
    ((attempt += 1))
  done
  if ! __agent_toolkit_relay_drained; then
    __agent_toolkit_kill_owned_session_groups
    __agent_toolkit_stop_group "$__agent_toolkit_source_pid"
    __agent_toolkit_stop_group "$__agent_toolkit_stdout_replay_pid"
    __agent_toolkit_stop_group "$__agent_toolkit_stderr_replay_pid"
  fi
  wait "$__agent_toolkit_stdout_replay_pid" 2>/dev/null || true
  wait "$__agent_toolkit_stderr_replay_pid" 2>/dev/null || true
}
__agent_toolkit_remove_path() {
  [[ -n "$1" ]] && rm -f -- "$1"
}
__agent_toolkit_cleanup() {
  local status="$?"
  set +e
  set +m
  if (( __agent_toolkit_cleanup_done == 0 )); then
    __agent_toolkit_cleanup_done=1
    __agent_toolkit_stop_group "$__agent_toolkit_source_pid"
    if (( __agent_toolkit_replay_done == 0 )); then
      __agent_toolkit_stop_group "$__agent_toolkit_stdout_replay_pid"
      __agent_toolkit_stop_group "$__agent_toolkit_stderr_replay_pid"
      __agent_toolkit_kill_owned_session_groups
    fi
    __agent_toolkit_remove_path "$__agent_toolkit_stdout_file"
    __agent_toolkit_remove_path "$__agent_toolkit_stderr_file"
    __agent_toolkit_remove_path "$marker_file"
  fi
  exit "$status"
}
__agent_toolkit_forward_signal() {
  local signal="$1" status="$2"
  __agent_toolkit_stop_group "$__agent_toolkit_source_pid"
  __agent_toolkit_stop_group "$__agent_toolkit_stdout_replay_pid"
  __agent_toolkit_stop_group "$__agent_toolkit_stderr_replay_pid"
  __agent_toolkit_kill_owned_session_groups
  exit "$status"
}
trap '__agent_toolkit_cleanup' EXIT
trap '__agent_toolkit_forward_signal SIGINT 130' INT
trap '__agent_toolkit_forward_signal SIGTERM 143' TERM
`.trim(),
    "__agent_toolkit_stdout_file=$(mktemp)",
    "__agent_toolkit_stderr_file=$(mktemp)",
    "rm -f -- \"$__agent_toolkit_stdout_file\" \"$__agent_toolkit_stderr_file\"",
    "mkfifo \"$__agent_toolkit_stdout_file\" \"$__agent_toolkit_stderr_file\"",
    "cat -- \"$__agent_toolkit_stdout_file\" &",
    "__agent_toolkit_stdout_replay_pid=$!",
    "cat -- \"$__agent_toolkit_stderr_file\" >&2 &",
    "__agent_toolkit_stderr_replay_pid=$!",
    "marker_file=$(mktemp)",
    "readonly marker_file __agent_toolkit_stdout_file __agent_toolkit_stderr_file",
    "__agent_toolkit_inside_test=",
    "test() { local status; __agent_toolkit_inside_test=1; builtin test \"$@\"; status=$?; __agent_toolkit_inside_test=; builtin printf '.\\n' >> \"$marker_file\" || true; return \"$status\"; }",
    "readonly -f test",
    "__agent_toolkit_record_builtin_test() { builtin printf '.\\n' >> \"$marker_file\" || true; }",
    "__agent_toolkit_fd_is_open() { local fd=\"$1\"; [[ \"$fd\" == \"-\" ]] && return 0; (builtin true >&\"$fd\") 2>/dev/null; }",
    "__agent_toolkit_fd_is_readable() { local fd=\"$1\"; [[ \"$fd\" == \"-\" ]] && return 0; (builtin true <&\"$fd\") 2>/dev/null; }",
    "__agent_toolkit_read_redirection_target() { local text=\"$1\" quote=\"\" char target=\"\"; while [[ -n \"$text\" ]]; do char=\"\${text:0:1}\"; if [[ -n \"$quote\" ]]; then if [[ \"$quote\" == \"'\" ]]; then if [[ \"$char\" == \"'\" ]]; then quote=\"\"; else target=\"$target$char\"; fi; text=\"\${text:1}\"; continue; fi; if [[ \"$char\" == '\"' ]]; then quote=\"\"; text=\"\${text:1}\"; continue; fi; if [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; [[ -n \"$text\" ]] || return 1; target=\"$target\${text:0:1}\"; text=\"\${text:1}\"; continue; fi; target=\"$target$char\"; text=\"\${text:1}\"; continue; fi; if [[ \"$char\" == \"'\" || \"$char\" == '\"' ]]; then quote=\"$char\"; text=\"\${text:1}\"; continue; fi; if [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; [[ -n \"$text\" ]] || return 1; target=\"$target\${text:0:1}\"; text=\"\${text:1}\"; continue; fi; if [[ \"$char\" =~ [[:space:]] ]] || [[ \"$char\" == \";\" || \"$char\" == \"|\" || \"$char\" == \"&\" || \"$char\" == \"(\" || \"$char\" == \")\" || \"$char\" == \"{\" || \"$char\" == \"}\" ]]; then break; fi; target=\"$target$char\"; text=\"\${text:1}\"; done; [[ -z \"$quote\" ]] || return 1; __agent_toolkit_redirection_target=\"$target\"; __agent_toolkit_redirection_rest=\"$text\"; return 0; }",
    "__agent_toolkit_has_unquoted_input_redirection() { local text=\"$1\" quote=\"\" char target; while [[ -n \"$text\" ]]; do char=\"\${text:0:1}\"; text=\"\${text:1}\"; if [[ -n \"$quote\" ]]; then if [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; elif [[ \"$char\" == \"$quote\" ]]; then quote=\"\"; fi; elif [[ \"$char\" == \"'\" || \"$char\" == '\"' ]]; then quote=\"$char\"; elif [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; elif [[ \"$char\" == '<' ]]; then if [[ \"\${text:0:1}\" == '<' ]]; then return 0; fi; while [[ \"\${text:0:1}\" =~ [[:space:]] ]]; do text=\"\${text:1}\"; done; if ! __agent_toolkit_read_redirection_target \"$text\"; then return 0; fi; target=\"$__agent_toolkit_redirection_target\"; text=\"$__agent_toolkit_redirection_rest\"; if [[ -z \"$target\" || \"$target\" != \"/dev/null\" && ! -f \"$target\" ]]; then return 0; fi; fi; done; return 1; }",
    "__agent_toolkit_has_unusable_output_redirection() { local text=\"$1\" quote=\"\" char target parent; while [[ -n \"$text\" ]]; do char=\"\${text:0:1}\"; text=\"\${text:1}\"; if [[ -n \"$quote\" ]]; then if [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; elif [[ \"$char\" == \"$quote\" ]]; then quote=\"\"; fi; elif [[ \"$char\" == \"'\" || \"$char\" == '\"' ]]; then quote=\"$char\"; elif [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; elif [[ \"$char\" == '>' ]]; then if [[ \"\${text:0:1}\" == '>' ]]; then text=\"\${text:1}\"; fi; while [[ \"\${text:0:1}\" =~ [[:space:]] ]]; do text=\"\${text:1}\"; done; if [[ \"\${text:0:1}\" == '&' ]]; then return 0; fi; if ! __agent_toolkit_read_redirection_target \"$text\"; then return 0; fi; target=\"$__agent_toolkit_redirection_target\"; text=\"$__agent_toolkit_redirection_rest\"; if [[ -z \"$target\" ]]; then return 0; fi; if [[ \"$target\" != \"/dev/null\" ]]; then [[ -d \"$target\" ]] && return 0; if [[ \"$target\" == */* ]]; then parent=\"\${target%/*}\"; [[ -n \"$parent\" ]] || parent=\"/\"; else parent=\".\"; fi; [[ -d \"$parent\" ]] || return 0; fi; fi; done; return 1; }",
    "__agent_toolkit_has_unquoted_input_redirection() {",
    "  local text=\"$1\" quote=\"\" char target fd",
    "  while [[ -n \"$text\" ]]; do",
    "    char=\"\${text:0:1}\"; text=\"\${text:1}\"",
    "    if [[ -n \"$quote\" ]]; then",
    "      if [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; elif [[ \"$char\" == \"$quote\" ]]; then quote=\"\"; fi",
    "    elif [[ \"$char\" == \"'\" || \"$char\" == '\"' ]]; then quote=\"$char\"",
    "    elif [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"",
    "    elif [[ \"$char\" == \"<\" ]]; then",
    "      if [[ \"\${text:0:1}\" == \"<\" ]]; then",
    "        [[ \"\${text:1:1}\" == \"<\" ]] || return 0",
    "        text=\"\${text:2}\"; while [[ \"\${text:0:1}\" =~ [[:space:]] ]]; do text=\"\${text:1}\"; done",
    "        __agent_toolkit_read_redirection_target \"$text\" || return 0; target=\"$__agent_toolkit_redirection_target\"; text=\"$__agent_toolkit_redirection_rest\"; [[ -n \"$target\" ]] || return 0",
    "      elif [[ \"\${text:0:1}\" == \"&\" ]]; then",
    "        text=\"\${text:1}\"; fd=\"\"; if [[ \"\${text:0:1}\" == \"-\" ]]; then text=\"\${text:1}\"; continue; fi",
    "        while [[ \"\${text:0:1}\" =~ [0-9] ]]; do fd=\"$fd\${text:0:1}\"; text=\"\${text:1}\"; done; [[ -n \"$fd\" ]] && __agent_toolkit_fd_is_open \"$fd\" || return 0",
    "      else",
    "        while [[ \"\${text:0:1}\" =~ [[:space:]] ]]; do text=\"\${text:1}\"; done; __agent_toolkit_read_redirection_target \"$text\" || return 0; target=\"$__agent_toolkit_redirection_target\"; text=\"$__agent_toolkit_redirection_rest\"; if [[ -z \"$target\" || \"$target\" != \"/dev/null\" && ! -f \"$target\" ]]; then return 0; fi",
    "      fi",
    "    fi",
    "  done",
    "  return 1",
    "}",
    "__agent_toolkit_has_unusable_output_redirection() {",
    "  local text=\"$1\" quote=\"\" char target parent fd force",
    "  while [[ -n \"$text\" ]]; do",
    "    char=\"\${text:0:1}\"; text=\"\${text:1}\"",
    "    if [[ -n \"$quote\" ]]; then",
    "      if [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"; elif [[ \"$char\" == \"$quote\" ]]; then quote=\"\"; fi",
    "    elif [[ \"$char\" == \"'\" || \"$char\" == '\"' ]]; then quote=\"$char\"",
    "    elif [[ \"$char\" == \"\\\\\" ]]; then text=\"\${text:1}\"",
    "    elif [[ \"$char\" == '>' ]]; then",
    "      force=\"\"; [[ \"\${text:0:1}\" == '>' ]] && text=\"\${text:1}\"; if [[ \"\${text:0:1}\" == '|' ]]; then force=\"1\"; text=\"\${text:1}\"; fi",
    "      while [[ \"\${text:0:1}\" =~ [[:space:]] ]]; do text=\"\${text:1}\"; done",
    "      if [[ \"\${text:0:1}\" == '&' ]]; then",
    "        text=\"\${text:1}\"; fd=\"\"; if [[ \"\${text:0:1}\" == \"-\" ]]; then text=\"\${text:1}\"; continue; fi",
    "        while [[ \"\${text:0:1}\" =~ [0-9] ]]; do fd=\"$fd\${text:0:1}\"; text=\"\${text:1}\"; done; [[ -n \"$fd\" ]] && __agent_toolkit_fd_is_open \"$fd\" || return 0; continue",
    "      fi",
    "      [[ \"$text\" == *'$'* || \"$text\" == *'`'* ]] && return 0",
    "      __agent_toolkit_read_redirection_target \"$text\" || return 0; target=\"$__agent_toolkit_redirection_target\"; text=\"$__agent_toolkit_redirection_rest\"; [[ -n \"$target\" ]] || return 0",
    "      if [[ \"$target\" == /dev/fd/* ]]; then fd=\"\${target#/dev/fd/}\"; [[ \"$fd\" =~ ^[0-9]+$ ]] && __agent_toolkit_fd_is_open \"$fd\" || return 0; fi",
    "      if [[ \"$target\" != \"/dev/null\" ]]; then",
    "        [[ -d \"$target\" ]] && return 0; if [[ \"$target\" == */* ]]; then parent=\"\${target%/*}\"; [[ -n \"$parent\" ]] || parent=\"/\"; else parent=\".\"; fi; [[ -d \"$parent\" ]] || return 0; [[ -z \"$force\" && -o noclobber && -f \"$target\" ]] && return 0",
    "      fi",
    "    fi",
    "  done",
    "  return 1",
    "}",
    "__agent_toolkit_debug_counter() { local command=\"$1\"; command=\"\${command//\\\"test\\\"/test}\"; command=\"\${command//\\'test\\'/test}\"; if [[ \"$command\" =~ ^([A-Za-z_][A-Za-z0-9_]*=[^[:space:];|(){}]+[[:space:]]+)*(builtin|command)([[:space:]]+(-p|--))*[[:space:]]+test([[:space:]]|$) ]] && [[ -z \"$__agent_toolkit_inside_test\" ]] && ! __agent_toolkit_has_unquoted_input_redirection \"$command\" && ! __agent_toolkit_has_unusable_output_redirection \"$command\"; then __agent_toolkit_record_builtin_test; fi; }",
    "__agent_toolkit_disable_nested_job_control() { builtin set +m; }",
    "__agent_toolkit_install_debug_trap() { local action=\"$1\"; if [[ \"$action\" == \"-\" ]]; then action=\"\"; fi; if [[ -n \"$action\" ]]; then builtin trap \"__agent_toolkit_debug_counter \\\"\\$BASH_COMMAND\\\"; __agent_toolkit_disable_nested_job_control; $action\" DEBUG; else builtin trap '__agent_toolkit_debug_counter \"$BASH_COMMAND\"; __agent_toolkit_disable_nested_job_control' DEBUG; fi; }",
    "trap() { if (( $# == 2 )) && [[ \"$2\" == \"DEBUG\" && \"$1\" != \"-p\" ]]; then __agent_toolkit_install_debug_trap \"$1\"; else builtin trap \"$@\"; fi; }",
    String.raw`__agent_toolkit_read_redirection_target() {
  local agentText="$1" agentQuote="" agentChar agentTarget="" agentQuoted="" agentExpandable=1
  while [[ -n "$agentText" ]]; do
    agentChar="\${agentText:0:1}"; agentText="\${agentText:1}"
    if [[ -n "$agentQuote" ]]; then
      if [[ "$agentQuote" == "'" ]]; then
        if [[ "$agentChar" == "'" ]]; then agentQuote=""; else agentTarget="$agentTarget$agentChar"; fi
      elif [[ "$agentChar" == '"' ]]; then
        agentQuote=""
      elif [[ "$agentChar" == "\\" ]]; then
        [[ -n "$agentText" ]] || return 1
        agentTarget="$agentTarget\${agentText:0:1}"; agentText="\${agentText:1}"; agentExpandable=0
      else
        agentTarget="$agentTarget$agentChar"
      fi
      continue
    fi
    if [[ "$agentChar" == "'" || "$agentChar" == '"' ]]; then
      agentQuote="$agentChar"; agentQuoted=1; [[ "$agentChar" == "'" ]] && agentExpandable=0
    elif [[ "$agentChar" == "\\" ]]; then
      [[ -n "$agentText" ]] || return 1
      agentTarget="$agentTarget\${agentText:0:1}"; agentText="\${agentText:1}"; agentQuoted=1; agentExpandable=0
    elif [[ "$agentChar" =~ [[:space:]] ]] || [[ "$agentChar" == ";" || "$agentChar" == "|" || "$agentChar" == "&" || "$agentChar" == "(" || "$agentChar" == ")" || "$agentChar" == "{" || "$agentChar" == "}" ]]; then
      break
    else
      agentTarget="$agentTarget$agentChar"
    fi
  done
  [[ -z "$agentQuote" ]] || return 1
  __agent_toolkit_redirection_target="$agentTarget"
  __agent_toolkit_redirection_quoted="$agentQuoted"
  __agent_toolkit_redirection_expandable="$agentExpandable"
  __agent_toolkit_redirection_rest="$agentText"
  return 0
}
__agent_toolkit_resolve_redirection_value() {
  local agentValue="$1" agentQuoted="$2" agentExpandable="\${3:-1}" agentVariableName agentPrefix agentSuffix
  if [[ "$agentExpandable" == 1 ]]; then
    while [[ "$agentValue" =~ ^(.*)\$\{([A-Za-z_][A-Za-z0-9_]*)\}(.*)$ ]]; do
      agentPrefix="\${BASH_REMATCH[1]}"; agentVariableName="\${BASH_REMATCH[2]}"; agentSuffix="\${BASH_REMATCH[3]}"
      declare -p "$agentVariableName" >/dev/null 2>&1 || return 1
      agentValue="$agentPrefix\${!agentVariableName}$agentSuffix"
    done
    while [[ "$agentValue" =~ ^(.*)\$([A-Za-z_][A-Za-z0-9_]*)(.*)$ ]]; do
      agentPrefix="\${BASH_REMATCH[1]}"; agentVariableName="\${BASH_REMATCH[2]}"; agentSuffix="\${BASH_REMATCH[3]}"
      declare -p "$agentVariableName" >/dev/null 2>&1 || return 1
      agentValue="$agentPrefix\${!agentVariableName}$agentSuffix"
    done
  fi
  [[ "$agentValue" == *'$'* || "$agentValue" == *$'\x60'* ]] && return 1
  [[ -n "$agentValue" ]] || return 1
  [[ "$agentQuoted" == 1 || "$agentValue" != *[[:space:]]* ]] || return 1
  __agent_toolkit_resolved_redirection_value="$agentValue"
  return 0
}
__agent_toolkit_fd_path_number() {
  local agentPath="$1"
  if [[ "$agentPath" =~ ^/dev/(fd|fd/\.|\./fd|fd/\.\./fd)/[0-9]+$ ]]; then
    __agent_toolkit_fd_path_value="\${agentPath##*/}"
  else
    __agent_toolkit_fd_path_value=""
  fi
}
__agent_toolkit_has_unquoted_input_redirection() {
  local agentText="$1" agentQuote="" agentChar agentTarget agentFd agentSourceFd agentMove agentDigits="" agentParent agentTargetFd agentOpenFds=":" agentClosedFds=":"
  for agentFd in 0 1 2; do
    if __agent_toolkit_fd_is_readable "$agentFd"; then agentOpenFds="$agentOpenFds$agentFd:"; else agentClosedFds="$agentClosedFds$agentFd:"; fi
  done
  while [[ -n "$agentText" ]]; do
    agentChar="\${agentText:0:1}"; agentText="\${agentText:1}"
    if [[ -n "$agentQuote" ]]; then
      if [[ "$agentChar" == "\\" ]]; then agentText="\${agentText:1}"; elif [[ "$agentChar" == "$agentQuote" ]]; then agentQuote=""; fi
      agentDigits=""; continue
    fi
    if [[ "$agentChar" == "'" || "$agentChar" == '"' ]]; then agentQuote="$agentChar"; agentDigits=""
    elif [[ "$agentChar" == "\\" ]]; then agentText="\${agentText:1}"; agentDigits=""
    elif [[ "$agentChar" =~ [0-9] ]]; then agentDigits="$agentDigits$agentChar"
    elif [[ "$agentChar" == '<' ]]; then
      agentFd="\${agentDigits:-0}"; agentDigits=""
      if [[ "\${agentText:0:1}" == '<' ]]; then
        agentText="\${agentText:1}"
        if [[ "\${agentText:0:1}" == '<' ]]; then agentText="\${agentText:1}"; while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done; __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"; [[ -n "$agentTarget" || "$__agent_toolkit_redirection_quoted" == 1 ]] || return 0
        else while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done; __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"; [[ -n "$agentTarget" ]] || return 0; fi
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentOpenFds="$agentOpenFds$agentFd:"; continue
      fi
      if [[ "\${agentText:0:1}" == '&' ]]; then
        agentText="\${agentText:1}"; while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"; agentMove=""
        if [[ "$agentTarget" == "-" ]]; then agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentClosedFds="$agentClosedFds$agentFd:"; continue; fi
        __agent_toolkit_resolve_redirection_value "$agentTarget" "$__agent_toolkit_redirection_quoted" "$__agent_toolkit_redirection_expandable" || return 0; agentSourceFd="$__agent_toolkit_resolved_redirection_value"
        if [[ "$agentSourceFd" =~ ^([0-9]+)-$ ]]; then agentMove=1; agentSourceFd="\${BASH_REMATCH[1]}"; fi
        [[ "$agentSourceFd" =~ ^[0-9]+$ ]] || return 0
        if [[ "$agentClosedFds" == *":$agentSourceFd:"* ]]; then return 0; fi
        if [[ "$agentOpenFds" != *":$agentSourceFd:"* ]]; then __agent_toolkit_fd_is_readable "$agentSourceFd" || return 0; agentOpenFds="$agentOpenFds$agentSourceFd:"; fi
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentOpenFds="$agentOpenFds$agentFd:"
        if [[ -n "$agentMove" ]]; then agentOpenFds="\${agentOpenFds//:$agentSourceFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentSourceFd:/:}"; agentClosedFds="$agentClosedFds$agentSourceFd:"; fi
      else
        while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"
        __agent_toolkit_resolve_redirection_value "$agentTarget" "$__agent_toolkit_redirection_quoted" "$__agent_toolkit_redirection_expandable" || return 0; agentTarget="$__agent_toolkit_resolved_redirection_value"
        __agent_toolkit_fd_path_number "$agentTarget"; agentTargetFd="$__agent_toolkit_fd_path_value"
        if [[ -n "$agentTargetFd" ]]; then
          if [[ "$agentClosedFds" == *":$agentTargetFd:"* ]]; then return 0; fi
          if [[ "$agentOpenFds" != *":$agentTargetFd:"* ]]; then __agent_toolkit_fd_is_readable "$agentTargetFd" || return 0; agentOpenFds="$agentOpenFds$agentTargetFd:"; fi
        elif [[ "$agentTarget" == /dev/fd/* || "$agentTarget" == /dev/./fd/* || "$agentTarget" == /dev/fd/../fd/* ]]; then return 0
        elif [[ "$agentTarget" != "/dev/null" ]]; then
          [[ -d "$agentTarget" ]] && return 0; [[ -f "$agentTarget" ]] || return 0
        fi
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentOpenFds="$agentOpenFds$agentFd:"
      fi
    elif [[ "$agentChar" == '>' ]]; then
      agentFd="\${agentDigits:-1}"; agentDigits=""
      if [[ "\${agentText:0:1}" == '>' ]]; then agentText="\${agentText:1}"; fi
      while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
      if [[ "\${agentText:0:1}" == '&' ]]; then
        agentText="\${agentText:1}"; while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"; agentMove=""
        if [[ "$agentTarget" == "-" ]]; then agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentClosedFds="$agentClosedFds$agentFd:"; continue; fi
        __agent_toolkit_resolve_redirection_value "$agentTarget" "$__agent_toolkit_redirection_quoted" "$__agent_toolkit_redirection_expandable" || return 0; agentSourceFd="$__agent_toolkit_resolved_redirection_value"
        if [[ "$agentSourceFd" =~ ^([0-9]+)-$ ]]; then agentMove=1; agentSourceFd="\${BASH_REMATCH[1]}"; fi
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"
        if [[ "$agentSourceFd" =~ ^[0-9]+$ ]] && __agent_toolkit_fd_is_readable "$agentSourceFd"; then agentOpenFds="$agentOpenFds$agentFd:"; else agentClosedFds="$agentClosedFds$agentFd:"; fi
        if [[ -n "$agentMove" ]]; then agentOpenFds="\${agentOpenFds//:$agentSourceFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentSourceFd:/:}"; agentClosedFds="$agentClosedFds$agentSourceFd:"; fi
      else
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentText="$__agent_toolkit_redirection_rest"
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentClosedFds="$agentClosedFds$agentFd:"
      fi
    else
      agentDigits=""
    fi
  done
  return 1
}
__agent_toolkit_has_unusable_output_redirection() {
  local agentText="$1" agentQuote="" agentChar agentTarget agentFd agentSourceFd agentMove agentDigits="" agentParent agentForce agentAppend agentTargetFd agentOpenFds=":" agentClosedFds=":"
  for agentFd in 0 1 2; do
    if __agent_toolkit_fd_is_open "$agentFd"; then agentOpenFds="$agentOpenFds$agentFd:"; else agentClosedFds="$agentClosedFds$agentFd:"; fi
  done
  while [[ -n "$agentText" ]]; do
    agentChar="\${agentText:0:1}"; agentText="\${agentText:1}"
    if [[ -n "$agentQuote" ]]; then
      if [[ "$agentChar" == "\\" ]]; then agentText="\${agentText:1}"; elif [[ "$agentChar" == "$agentQuote" ]]; then agentQuote=""; fi
      agentDigits=""; continue
    fi
    if [[ "$agentChar" == "'" || "$agentChar" == '"' ]]; then agentQuote="$agentChar"; agentDigits=""
    elif [[ "$agentChar" == "\\" ]]; then agentText="\${agentText:1}"; agentDigits=""
    elif [[ "$agentChar" =~ [0-9] ]]; then agentDigits="$agentDigits$agentChar"
    elif [[ "$agentChar" == '>' ]]; then
      agentFd="\${agentDigits:-1}"; agentDigits=""; agentAppend=""; agentForce=""
      if [[ "\${agentText:0:1}" == '>' ]]; then agentAppend=1; agentText="\${agentText:1}"; fi
      if [[ "\${agentText:0:1}" == '|' ]]; then [[ -z "$agentAppend" ]] || return 0; agentForce=1; agentText="\${agentText:1}"; fi
      while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
      if [[ "\${agentText:0:1}" == '&' ]]; then
        agentText="\${agentText:1}"; while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"; agentMove=""
        if [[ "$agentTarget" == "-" ]]; then agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentClosedFds="$agentClosedFds$agentFd:"; continue; fi
        __agent_toolkit_resolve_redirection_value "$agentTarget" "$__agent_toolkit_redirection_quoted" "$__agent_toolkit_redirection_expandable" || return 0; agentSourceFd="$__agent_toolkit_resolved_redirection_value"
        if [[ "$agentSourceFd" =~ ^([0-9]+)-$ ]]; then agentMove=1; agentSourceFd="\${BASH_REMATCH[1]}"; fi
        [[ "$agentSourceFd" =~ ^[0-9]+$ ]] || return 0
        if [[ "$agentClosedFds" == *":$agentSourceFd:"* ]]; then return 0; fi
        if [[ "$agentOpenFds" != *":$agentSourceFd:"* ]]; then __agent_toolkit_fd_is_open "$agentSourceFd" || return 0; agentOpenFds="$agentOpenFds$agentSourceFd:"; fi
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentOpenFds="$agentOpenFds$agentFd:"
        if [[ -n "$agentMove" ]]; then agentOpenFds="\${agentOpenFds//:$agentSourceFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentSourceFd:/:}"; agentClosedFds="$agentClosedFds$agentSourceFd:"; fi
        continue
      fi
      __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"
      __agent_toolkit_resolve_redirection_value "$agentTarget" "$__agent_toolkit_redirection_quoted" "$__agent_toolkit_redirection_expandable" || return 0; agentTarget="$__agent_toolkit_resolved_redirection_value"
      __agent_toolkit_fd_path_number "$agentTarget"; agentTargetFd="$__agent_toolkit_fd_path_value"
      if [[ -n "$agentTargetFd" ]]; then
        if [[ "$agentClosedFds" == *":$agentTargetFd:"* ]]; then return 0; fi
        if [[ "$agentTargetFd" == 0 ]] && ! (builtin true > "$agentTarget") 2>/dev/null; then return 0; fi
        if [[ "$agentOpenFds" != *":$agentTargetFd:"* ]]; then __agent_toolkit_fd_is_open "$agentTargetFd" || return 0; agentOpenFds="$agentOpenFds$agentTargetFd:"; fi
      elif [[ "$agentTarget" == /dev/fd/* || "$agentTarget" == /dev/./fd/* || "$agentTarget" == /dev/fd/../fd/* ]]; then return 0
      elif [[ "$agentTarget" != "/dev/null" ]]; then
        [[ -d "$agentTarget" ]] && return 0
        if [[ "$agentTarget" == */* ]]; then agentParent="\${agentTarget%/*}"; [[ -n "$agentParent" ]] || agentParent="/"; else agentParent="."; fi
        [[ -d "$agentParent" ]] || return 0
        [[ -z "$agentForce" && -z "$agentAppend" && -o noclobber && -f "$agentTarget" ]] && return 0
      fi
      agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentOpenFds="$agentOpenFds$agentFd:"
    elif [[ "$agentChar" == '<' ]]; then
      agentFd="\${agentDigits:-0}"; agentDigits=""
      if [[ "\${agentText:0:1}" == '<' ]]; then agentText="\${agentText:1}"; fi
      while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
      if [[ "\${agentText:0:1}" == '&' ]]; then
        agentText="\${agentText:1}"; while [[ "\${agentText:0:1}" =~ [[:space:]] ]]; do agentText="\${agentText:1}"; done
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentTarget="$__agent_toolkit_redirection_target"; agentText="$__agent_toolkit_redirection_rest"; agentMove=""
        if [[ "$agentTarget" == "-" ]]; then agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentClosedFds="$agentClosedFds$agentFd:"; continue; fi
        __agent_toolkit_resolve_redirection_value "$agentTarget" "$__agent_toolkit_redirection_quoted" "$__agent_toolkit_redirection_expandable" || return 0; agentSourceFd="$__agent_toolkit_resolved_redirection_value"
        if [[ "$agentSourceFd" =~ ^([0-9]+)-$ ]]; then agentMove=1; agentSourceFd="\${BASH_REMATCH[1]}"; fi
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"
        if [[ "$agentSourceFd" =~ ^[0-9]+$ ]] && __agent_toolkit_fd_is_open "$agentSourceFd"; then agentOpenFds="$agentOpenFds$agentFd:"; else agentClosedFds="$agentClosedFds$agentFd:"; fi
        if [[ -n "$agentMove" ]]; then agentOpenFds="\${agentOpenFds//:$agentSourceFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentSourceFd:/:}"; agentClosedFds="$agentClosedFds$agentSourceFd:"; fi
      else
        __agent_toolkit_read_redirection_target "$agentText" || return 0; agentText="$__agent_toolkit_redirection_rest"
        agentOpenFds="\${agentOpenFds//:$agentFd:/:}"; agentClosedFds="\${agentClosedFds//:$agentFd:/:}"; agentClosedFds="$agentClosedFds$agentFd:"
      fi
    else
      agentDigits=""
    fi
  done
  return 1
}
`.replaceAll("\\${", "${"),
    "readonly -f test trap __agent_toolkit_signal_group __agent_toolkit_stop_group __agent_toolkit_process_active __agent_toolkit_kill_owned_session_groups __agent_toolkit_session_id __agent_toolkit_session_groups __agent_toolkit_relay_drained __agent_toolkit_wait_for_relays __agent_toolkit_remove_path __agent_toolkit_cleanup __agent_toolkit_forward_signal __agent_toolkit_record_builtin_test __agent_toolkit_fd_is_open __agent_toolkit_fd_is_readable __agent_toolkit_read_redirection_target __agent_toolkit_resolve_redirection_value __agent_toolkit_fd_path_number __agent_toolkit_has_unquoted_input_redirection __agent_toolkit_has_unusable_output_redirection __agent_toolkit_debug_counter __agent_toolkit_disable_nested_job_control __agent_toolkit_install_debug_trap",
    "set +e",
    "(",
    "  set -euo pipefail",
    "  set -T",
    "  set +m",
    "  __agent_toolkit_install_debug_trap \"\"",
    "  source \"$0\"",
    ") >\"$__agent_toolkit_stdout_file\" 2>\"$__agent_toolkit_stderr_file\" &",
    "__agent_toolkit_source_pid=$!",
    "set +m",
    "wait \"$__agent_toolkit_source_pid\"",
    "exit_status=$?",
    "__agent_toolkit_stop_group \"$__agent_toolkit_source_pid\"",
    "__agent_toolkit_wait_for_relays",
    "__agent_toolkit_replay_done=1",
    "set -e",
    "count=$(wc -l < \"$marker_file\")",
    "if ((exit_status == 0 && count == 0)); then exit_status=1; fi",
    "if ((exit_status == 0)); then builtin printf \"TESTS_RUN=%s\\n\" \"$count\"; fi",
    "exit \"$exit_status\"",
  ].join("\n");
  return [
    "bash",
    "-c",
    shellCommand,
    test.path,
  ];
}

function signalProcessGroup(pid: number, signal: NodeJS.Signals): void {
  if (process.platform === "win32") {
    try {
      process.kill(pid, signal);
    } catch {
      // The child may have exited between the signal and cleanup.
    }
    return;
  }
  try {
    process.kill(-pid, signal);
  } catch {
    try {
      process.kill(pid, signal);
    } catch {
      // The child may have exited between the signal and cleanup.
    }
  }
}

type ProcessIdentity = { pid: number; sessionId: number; processGroupId: number };

function processIdentity(pid: number): ProcessIdentity | undefined {
  if (process.platform === "win32") return undefined;
  for (const sessionField of ["sess", "sid"]) {
    const result = Bun.spawnSync(["ps", "-o", `pid=,${sessionField}=,pgid=`, "-p", String(pid)], {
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) continue;
    const fields = new TextDecoder().decode(result.stdout).trim().split(/\s+/).map(Number);
    if (fields.length >= 3 && fields.every((value) => Number.isInteger(value) && value > 0)) {
      return { pid: fields[0]!, sessionId: fields[1]!, processGroupId: fields[2]! };
    }
  }
  return undefined;
}

function processSessionMembers(sessionId: number): ProcessIdentity[] {
  if (process.platform === "win32") return [];
  for (const sessionField of ["sess", "sid"]) {
    const result = Bun.spawnSync(["ps", "-axo", `pid=,${sessionField}=,pgid=`], {
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) continue;
    return new TextDecoder().decode(result.stdout)
      .split("\n")
      .map((line) => line.trim().split(/\s+/).map(Number))
      .filter((fields) => fields.length >= 3 && fields.every((value) => Number.isInteger(value) && value > 0))
      .map(([pid, memberSessionId, processGroupId]) => ({ pid: pid!, sessionId: memberSessionId!, processGroupId: processGroupId! }))
      .filter((member) => member.sessionId === sessionId);
  }
  return [];
}

function signalProcessSession(sessionId: number, signal: NodeJS.Signals): void {
  if (process.platform === "win32") return;
  const runner = processIdentity(process.pid);
  if (runner?.sessionId === sessionId) return;
  const members = processSessionMembers(sessionId);
  const runnerGroup = runner?.processGroupId;
  const groups = new Set(members.map((member) => member.processGroupId));
  for (const group of groups) {
    if (group === runnerGroup) continue;
    try {
      process.kill(-group, signal);
    } catch {
      // The group may have exited between enumeration and signaling.
    }
  }
  for (const member of members) {
    if (member.pid === process.pid || member.processGroupId === runnerGroup) continue;
    try {
      process.kill(member.pid, signal);
    } catch {
      // The process may have exited between enumeration and signaling.
    }
  }
}

function signalOwnedProcess(pid: number, sessionId: number | undefined, signal: NodeJS.Signals): void {
  const runnerSessionId = processIdentity(process.pid)?.sessionId;
  if (sessionId !== undefined && sessionId !== runnerSessionId) {
    signalProcessSession(sessionId, signal);
  }
  if (sessionId === undefined || sessionId !== runnerSessionId) {
    signalProcessGroup(pid, signal);
  }
}

const OUTPUT_DRAIN_DEADLINE_MS = 250;
const OUTPUT_TERM_GRACE_MS = 250;
const OUTPUT_KILL_GRACE_MS = 250;

type OutputCapture = {
  promise: Promise<string>;
  cancel: () => Promise<void>;
  snapshot: () => string;
};

function captureProcessOutput(stream: ReadableStream<Uint8Array<ArrayBuffer>>): OutputCapture {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let output = "";
  const promise = (async () => {
    try {
      while (true) {
        const chunk = await reader.read();
        if (chunk.done) break;
        output += decoder.decode(chunk.value, { stream: true });
      }
      output += decoder.decode();
      return output;
    } finally {
      reader.releaseLock();
    }
  })();
  return {
    promise,
    cancel: async () => {
      try {
        await reader.cancel();
      } catch {
        // The reader may already have released its lock after EOF.
      }
    },
    snapshot: () => output,
  };
}

async function outputSettled(captures: OutputCapture[], timeoutMs: number): Promise<boolean> {
  return Promise.race([
    Promise.all(captures.map((capture) => capture.promise)).then(() => true, () => true),
    Bun.sleep(timeoutMs).then(() => false),
  ]);
}

async function collectedOutput(capture: OutputCapture): Promise<string> {
  try {
    return await capture.promise;
  } catch {
    return capture.snapshot();
  }
}

async function drainProcessOutput(child: Bun.Subprocess, sessionId: number | undefined, captures: OutputCapture[]): Promise<[string, string]> {
  if (!(await outputSettled(captures, OUTPUT_DRAIN_DEADLINE_MS))) {
    signalOwnedProcess(child.pid, sessionId, "SIGTERM");
    if (!(await outputSettled(captures, OUTPUT_TERM_GRACE_MS))) {
      signalOwnedProcess(child.pid, sessionId, "SIGKILL");
      if (!(await outputSettled(captures, OUTPUT_KILL_GRACE_MS))) {
        await Promise.all(captures.map((capture) => capture.cancel()));
      }
    }
  }
  return [await collectedOutput(captures[0]!), await collectedOutput(captures[1]!)];
}

export async function runTest(test: TestFile): Promise<number> {
  if (!isPathInside(REPO_ROOT, test.path) || !isDirectRegularFile(test.path)) {
    console.error(`ERROR refusing to run a test outside the regular repository file boundary: ${test.path}`);
    return 1;
  }
  console.log(`::group::${relativePath(test.path)}`);
  let child: Bun.Subprocess | undefined;
  let childSessionId: number | undefined;
  let pendingSignal: NodeJS.Signals | undefined;
  let escalationTimer: ReturnType<typeof setTimeout> | undefined;
  const forward = (signal: NodeJS.Signals) => {
    if (child === undefined) {
      pendingSignal ??= signal;
      return;
    }
    const target = child;
    signalOwnedProcess(target.pid, childSessionId, signal);
    escalationTimer ??= setTimeout(() => signalOwnedProcess(target.pid, childSessionId, "SIGKILL"), 1000);
  };
  const forwardInterrupt = () => forward("SIGINT");
  const forwardTermination = () => forward("SIGTERM");
  process.on("SIGINT", forwardInterrupt);
  process.on("SIGTERM", forwardTermination);
  let exitCode: number;
  let childExited = false;
  let out = "";
  let err = "";
  try {
    const spawnedChild = Bun.spawn(commandForTest(test), {
      cwd: REPO_ROOT,
      stdout: "pipe",
      stderr: "pipe",
      detached: true,
    });
    child = spawnedChild;
    const identity = processIdentity(spawnedChild.pid);
    childSessionId = identity?.sessionId;
    if (pendingSignal !== undefined) {
      const signal = pendingSignal;
      pendingSignal = undefined;
      forward(signal);
    }
    const captures = [captureProcessOutput(spawnedChild.stdout), captureProcessOutput(spawnedChild.stderr)];
    exitCode = await spawnedChild.exited;
    childExited = true;
    [out, err] = await drainProcessOutput(spawnedChild, childSessionId, captures);
    spawnedChild.unref();
  } finally {
    if (escalationTimer !== undefined) clearTimeout(escalationTimer);
    if (child !== undefined && !childExited) {
      signalOwnedProcess(child.pid, childSessionId, "SIGKILL");
      child.unref();
    }
    process.off("SIGINT", forwardInterrupt);
    process.off("SIGTERM", forwardTermination);
  }
  process.stdout.write(out);
  process.stderr.write(err);
  console.log("::endgroup::");
  const count = executedTestCount(test.kind, `${out}\n${err}`);
  if (exitCode === 0 && !hasExecutedTests(test.kind, `${out}\n${err}`, count)) {
    console.error(`ERROR test command reported zero executed tests: ${test.path}`);
    return 1;
  }
  return exitCode;
}

function parseScope(args: string[]): TestScope {
  if (args.length === 1 && args[0].startsWith("--scope=")) {
    const value = args[0].slice("--scope=".length);
    if (value === "repo" || value === "skills") return value;
  }
  if (args.length === 2 && args[0] === "--scope") {
    const value = args[1];
    if (value === "repo" || value === "skills") return value;
  }
  throw new Error("usage: test_runner.ts --scope repo|skills");
}

export async function main(args: string[] = Bun.argv.slice(2)): Promise<number> {
  let scope: TestScope;
  try {
    scope = parseScope(args);
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 2;
  }
  const tests = discoverTests(REPO_ROOT, scope);
  try {
    ensureTestSelection(tests, scope);
    ensureTestSources(tests);
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 1;
  }
  console.log(`Discovered ${tests.length} ${scope} test file(s)`);
  for (const test of tests) {
    const exitCode = await runTest(test);
    if (exitCode !== 0) return exitCode;
  }
  return 0;
}

if (import.meta.main) {
  process.exitCode = await main();
}
