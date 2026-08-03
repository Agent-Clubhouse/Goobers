import os


def test_service_and_python_environment():
    assert 2 + 2 == 4
    assert os.environ["VIRTUAL_ENV"] == "/custom/python-venv"
    assert os.environ["PYTHONPATH"] == "/custom/python-modules"
    assert os.environ["PIP_CACHE_DIR"] == "/custom/python-pip-cache"
